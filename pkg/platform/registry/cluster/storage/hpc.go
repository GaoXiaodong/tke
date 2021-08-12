/*
 * Copyright 2019 THL A29 Limited, a Tencent company.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package storage

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"time"
	platforminternalclient "tkestack.io/tke/api/client/clientset/internalversion/typed/platform/internalversion"
	platformv1 "tkestack.io/tke/api/platform/v1"
	"tkestack.io/tke/pkg/apiserver/authentication"
	clusterprovider "tkestack.io/tke/pkg/platform/provider/cluster"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	"k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	netutil "k8s.io/apimachinery/pkg/util/net"

	"tkestack.io/tke/api/platform"
	"tkestack.io/tke/pkg/platform/util"
)

// HPCREST implements proxy LogCollector request to cluster of user.
type HPCREST struct {
	rest.Storage
	store          *registry.Store
	platformClient platforminternalclient.PlatformInterface
}

// HPCProxyHandler impletement  ServeHTTP()
type HPCProxyHandler struct {
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

// ConnectMethods returns the list of HTTP methods that can be proxied
func (r *HPCREST) ConnectMethods() []string {
	return []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}
}

// NewConnectOptions returns versioned resource that represents proxy parameters
func (r *HPCREST) NewConnectOptions() (runtime.Object, bool, string) {
	return &platform.HPCProxyOptions{}, false, ""
}

// Connect returns a handler for the kube-apiserver proxy
func (r *HPCREST) Connect(ctx context.Context, clusterName string, opts runtime.Object, responder rest.Responder) (http.Handler, error) {
	clusterObject, err := r.store.Get(ctx, clusterName, &metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	cluster := clusterObject.(*platform.Cluster)
	proxyOpts := opts.(*platform.HPCProxyOptions)

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
	return &HPCProxyHandler{
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

// New creates a new LogCollector proxy options object
func (r *HPCREST) New() runtime.Object {
	return &platform.HPCProxyOptions{}
}

func (h *HPCProxyHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	loc := *h.location
	loc.RawQuery = req.URL.RawQuery

	prefix := "/apis/autoscaling.cloud.tencent.com/v1"

	if len(h.action) > 0 {
		h.serveAction(w, req)
		return
	}

	if len(h.namespace) == 0 { // default: all namespaces
		loc.Path = fmt.Sprintf("%s/horizontalpodcronscalers", prefix)
	} else if len(h.name) == 0 { // specified namespace
		loc.Path = fmt.Sprintf("%s/namespaces/%s/horizontalpodcronscalers", prefix, h.namespace)
	} else { // specified resource in target namespace
		loc.Path = fmt.Sprintf("%s/namespaces/%s/horizontalpodcronscalers/%s", prefix, h.namespace, h.name)
	}

	// WithContext creates a shallow clone of the request with the new context.
	newReq := req.WithContext(context.Background())
	newReq.Header = netutil.CloneHeader(req.Header)
	newReq.URL = &loc
	if h.token != "" {
		newReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", strings.TrimSpace(h.token)))
	}

	reserveProxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: h.location.Scheme, Host: h.location.Host})
	reserveProxy.Transport = h.transport
	reserveProxy.FlushInterval = 100 * time.Millisecond
	reserveProxy.ServeHTTP(w, newReq)
}

func (h *HPCProxyHandler) serveAction(w http.ResponseWriter, req *http.Request) {
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
	default:
		responsewriters.WriteRawJSON(http.StatusBadRequest, errors.NewBadRequest("unsupported action"), w)
	}
}

var (
	hpcResource = schema.GroupVersionResource{Group: "autoscaling.cloud.tencent.com", Version: "v1", Resource: "horizontalpodcronscalers"}
)

// Get retrieves the object from the storage. It is required to support Patch.
func (h *HPCProxyHandler) getEventList(ctx context.Context) (*corev1.EventList, error) {
	return getEvents(ctx, h.cluster, h.clusterCredential, hpcResource, "HorizontalPodCronscaler", h.namespace, h.name)
}

func getEvents(ctx context.Context, cluster *platform.Cluster, credential *platform.ClusterCredential, resource schema.GroupVersionResource, kind, namespace, name string) (*corev1.EventList, error) {
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

	sort.Sort(events)

	return &corev1.EventList{
		Items: events,
	}, nil
}
