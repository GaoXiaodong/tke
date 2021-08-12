/*
 * Tencent is pleased to support the open source community by making TKE
 * available.
 *
 * Copyright (C) 2018 THL A29 Limited, a Tencent company. All rights reserved.
 *
 * Licensed under the BSD 3-Clause License (the "License"); you may not use this
 * file except in compliance with the License. You may obtain a copy of the
 * License at
 *
 * https://opensource.org/licenses/BSD-3-Clause
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS, WITHOUT
 * WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
 * License for the specific language governing permissions and limitations under
 * the License.
 */

package storage

import (
	"context"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	netUtil "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	"k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"time"
	platforminternalclient "tkestack.io/tke/api/client/clientset/internalversion/typed/platform/internalversion"
	"tkestack.io/tke/api/platform"
	platformv1 "tkestack.io/tke/api/platform/v1"
	"tkestack.io/tke/pkg/apiserver/authentication"
	clusterprovider "tkestack.io/tke/pkg/platform/provider/cluster"
	"tkestack.io/tke/pkg/platform/util"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	PODS   = "pods"
	EVENTS = "events"
)

// GameAppREST implements proxy gameapp request to cluster of user.
type GameAppREST struct {
	rest.Storage
	store          *registry.Store
	platformClient platforminternalclient.PlatformInterface
}

// ConnectMethods returns the list of HTTP methods that can be proxied
func (r *GameAppREST) ConnectMethods() []string {
	return []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}
}

// NewConnectOptions returns versioned resource that represents proxy parameters
func (r *GameAppREST) NewConnectOptions() (runtime.Object, bool, string) {
	return &platform.GameAppProxyOptions{}, false, ""
}

// Connect returns a handler for the gameapp-api proxy
func (r *GameAppREST) Connect(ctx context.Context, clusterName string, opts runtime.Object, responder rest.Responder) (http.Handler, error) {
	clusterObject, err := r.store.Get(ctx, clusterName, &metaV1.GetOptions{})
	if err != nil {
		return nil, err
	}
	cluster := clusterObject.(*platform.Cluster)
	proxyOpts := opts.(*platform.GameAppProxyOptions)

	if len(proxyOpts.Action) != 0 {
		if proxyOpts.Action != PODS && proxyOpts.Action != EVENTS {
			return nil, errors.NewBadRequest("action invalid")
		}
	}

	location, transport, token, err := util.APIServerLocationByCluster(ctx, cluster, r.platformClient)
	if err != nil {
		return nil, err
	}
	provider, err := clusterprovider.GetProvider(cluster.Spec.Type)
	if err != nil {
		return nil, err
	}

	username, _ := authentication.UsernameAndTenantID(ctx)
	credential, err := provider.GetClusterCredential(ctx, r.platformClient, cluster, username)
	if err != nil {
		return nil, err
	}
	return &gameAppProxyHandler{
		responder:         responder,
		location:          location,
		transport:         transport,
		token:             token,
		namespace:         proxyOpts.Namespace,
		name:              proxyOpts.Name,
		action:            proxyOpts.Action,
		cluster:           cluster,
		clusterCredential: credential,
		platformClient:    r.platformClient,
	}, nil
}

// New creates a new gameapp proxy options object
func (r *GameAppREST) New() runtime.Object {
	return &platform.GameAppProxyOptions{}
}

type gameAppProxyHandler struct {
	responder         rest.Responder
	transport         http.RoundTripper
	location          *url.URL
	token             string
	namespace         string
	name              string
	action            string
	cluster           *platform.Cluster
	clusterCredential *platform.ClusterCredential
	platformClient    platforminternalclient.PlatformInterface
}

func (h *gameAppProxyHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	loc := *h.location
	loc.RawQuery = req.URL.RawQuery

	prefix := "/apis/game.scr.ied.com/v1"

	if len(h.action) > 0 {
		h.serveAction(w, req)
		return
	}

	if len(h.namespace) == 0 && len(h.name) == 0 {
		loc.Path = fmt.Sprintf("%s/gameapps", prefix)
	} else if len(h.name) == 0 {
		loc.Path = fmt.Sprintf("%s/namespaces/%s/gameapps", prefix, h.namespace)
	} else {
		loc.Path = fmt.Sprintf("%s/namespaces/%s/gameapps/%s", prefix, h.namespace, h.name)
	}

	// WithContext creates a shallow clone of the request with the new context.
	newReq := req.WithContext(context.Background())
	newReq.Header = netUtil.CloneHeader(req.Header)
	newReq.URL = &loc
	if h.token != "" {
		newReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", strings.TrimSpace(h.token)))
	}

	reserveProxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: h.location.Scheme, Host: h.location.Host})
	reserveProxy.Transport = h.transport
	reserveProxy.FlushInterval = 100 * time.Millisecond
	reserveProxy.ServeHTTP(w, newReq)
}

func (h *gameAppProxyHandler) serveAction(w http.ResponseWriter, req *http.Request) {
	if len(h.namespace) == 0 || len(h.name) == 0 {
		responsewriters.WriteRawJSON(http.StatusBadRequest, errors.NewBadRequest("namespace and name must be specified"), w)
		return
	}
	switch h.action {
	case string(EVENTS):
		if eventList, err := h.getEventList(req.Context()); err != nil {
			responsewriters.WriteRawJSON(http.StatusInternalServerError, errors.NewInternalError(err), w)
		} else {
			responsewriters.WriteRawJSON(http.StatusOK, eventList, w)
		}
	case string(PODS):
		if groupList, err := h.getPods(req.Context()); err != nil {
			responsewriters.WriteRawJSON(http.StatusInternalServerError, errors.NewInternalError(err), w)
		} else {
			responsewriters.WriteRawJSON(http.StatusOK, groupList, w)
		}
	default:
		responsewriters.WriteRawJSON(http.StatusBadRequest, errors.NewBadRequest("unsupported action"), w)
	}
}

var (
	gameappResource = schema.GroupVersionResource{Group: "game.scr.ied.com", Version: "v1", Resource: "GameApp"}
)

// Get retrieves the object from the storage. It is required to support Patch.
func (h *gameAppProxyHandler) getEventList(ctx context.Context) (*corev1.EventList, error) {
	return getGameAppEvents(ctx, h.cluster, h.clusterCredential, gameappResource, "GameApp", h.namespace, h.name)
}

func getGameAppEvents(ctx context.Context, cluster *platform.Cluster, credential *platform.ClusterCredential, resource schema.GroupVersionResource, kind, namespace, name string) (*corev1.EventList, error) {
	var clusterv1 platformv1.Cluster
	if err := platformv1.Convert_platform_Cluster_To_v1_Cluster(cluster, &clusterv1, nil); err != nil {
		return nil, err
	}
	var clusterCredential platformv1.ClusterCredential
	if err := platformv1.Convert_platform_ClusterCredential_To_v1_ClusterCredential(credential, &clusterCredential, nil); err != nil {
		return nil, err
	}
	dynamicClient, err := util.BuildExternalDynamicClientSet(&clusterv1, &clusterCredential)
	if err != nil {
		return nil, err
	}
	obj, err := dynamicClient.Resource(resource).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	kubeclient, err := util.BuildClientSet(ctx, cluster, credential)
	if err != nil {
		return nil, err
	}

	eventList, err := util.GetEvents(ctx, kubeclient, string(obj.GetUID()), obj.GetNamespace(), obj.GetName(), kind)
	if err != nil {
		return nil, err
	}

	var events util.EventSlice
	for _, event := range eventList.Items {
		events = append(events, event)
	}

	label := obj.GetLabels()
	selector := labels.NewSelector()
	for k, v := range label {
		r, err := labels.NewRequirement(k, selection.Equals, []string{v})
		if err != nil {
			return nil, err
		}
		selector = selector.Add(*r)
	}

	// list all of the pod, by gameapp labels
	listOptions := metaV1.ListOptions{LabelSelector: selector.String()}
	podAllList, err := kubeclient.CoreV1().Pods(namespace).List(ctx, listOptions)
	if err != nil {
		return nil, errors.NewInternalError(err)
	}

	for _, pod := range podAllList.Items {
		for _, podReferences := range pod.ObjectMeta.OwnerReferences {
			if (podReferences.Kind == kind) && (podReferences.Name == obj.GetName()) {
				podEvents, err := util.GetEvents(ctx, kubeclient, string(pod.GetUID()), pod.GetNamespace(), pod.GetName(), "Pod")
				if err != nil {
					return nil, err
				}
				for _, podEvent := range podEvents.Items {
					events = append(events, podEvent)
				}
			}
		}
	}

	sort.Sort(events)

	return &corev1.EventList{
		Items: events,
	}, nil
}

// Get retrieves the object from the storage. It is required to support Patch.
func (h *gameAppProxyHandler) getPods(ctx context.Context) (*corev1.PodList, error) {
	return getPods(ctx, h.cluster, h.clusterCredential, gameappResource, "GameApp", h.namespace, h.name)
}

func getPods(ctx context.Context, cluster *platform.Cluster, credential *platform.ClusterCredential, resource schema.GroupVersionResource, kind, namespace, name string) (*corev1.PodList, error) {
	if len(namespace) == 0 || len(name) == 0 {
		return nil, errors.NewBadRequest("namespace and name must be specified")
	}

	var clusterv1 platformv1.Cluster
	if err := platformv1.Convert_platform_Cluster_To_v1_Cluster(cluster, &clusterv1, nil); err != nil {
		return nil, err
	}
	var clusterCredential platformv1.ClusterCredential
	if err := platformv1.Convert_platform_ClusterCredential_To_v1_ClusterCredential(credential, &clusterCredential, nil); err != nil {
		return nil, err
	}

	dynamicClient, err := util.BuildExternalDynamicClientSet(&clusterv1, &clusterCredential)
	if err != nil {
		return nil, err
	}
	obj, err := dynamicClient.Resource(resource).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	kubeclient, err := util.BuildClientSet(ctx, cluster, credential)
	if err != nil {
		return nil, err
	}

	label := obj.GetLabels()
	selector := labels.NewSelector()
	for k, v := range label {
		r, err := labels.NewRequirement(k, selection.Equals, []string{v})
		if err != nil {
			return nil, err
		}
		selector = selector.Add(*r)
	}

	// list all of the pod, by gameapp labels
	listOptions := metaV1.ListOptions{LabelSelector: selector.String()}
	podAllList, err := kubeclient.CoreV1().Pods(namespace).List(ctx, listOptions)
	if err != nil {
		return nil, errors.NewInternalError(err)
	}

	podList := &corev1.PodList{}
	for _, pod := range podAllList.Items {
		for _, podReferences := range pod.ObjectMeta.OwnerReferences {
			if (podReferences.Kind == kind) && (podReferences.Name == obj.GetName()) {
				podList.Items = append(podList.Items, pod)
			}
		}
	}
	return podList, nil
}
