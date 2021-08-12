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
	"strings"
	"time"
	platforminternalclient "tkestack.io/tke/api/client/clientset/internalversion/typed/platform/internalversion"
	"tkestack.io/tke/pkg/apiserver/authentication"
	clusterprovider "tkestack.io/tke/pkg/platform/provider/cluster"
	"tkestack.io/tke/pkg/platform/util"

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
)

// OLMREST implements proxy LogCollector request to cluster of user.
type OLMREST struct {
	rest.Storage
	store          *registry.Store
	platformClient platforminternalclient.PlatformInterface
}

// OLMProxyHandler impletement  ServeHTTP()
type OLMProxyHandler struct {
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
func (r *OLMREST) ConnectMethods() []string {
	return []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}
}

// NewConnectOptions returns versioned resource that represents proxy parameters
func (r *OLMREST) NewConnectOptions() (runtime.Object, bool, string) {
	return &platform.OLMProxyOptions{}, false, ""
}

// Connect returns a handler for the kube-apiserver proxy
func (r *OLMREST) Connect(ctx context.Context, clusterName string, opts runtime.Object, responder rest.Responder) (http.Handler, error) {
	clusterObject, err := r.store.Get(ctx, clusterName, &metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	cluster := clusterObject.(*platform.Cluster)
	if err := util.FilterCluster(ctx, cluster); err != nil {
		return nil, err
	}
	proxyOpts := opts.(*platform.OLMProxyOptions)

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
	return &OLMProxyHandler{
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

// New creates a new Catalog Collector proxy options object
func (r *OLMREST) New() runtime.Object {
	return &platform.OLMProxyOptions{}
}

func (h *OLMProxyHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	loc := *h.location
	loc.RawQuery = req.URL.RawQuery

	prefix := "/apis/operators.coreos.com/v1alpha1/namespaces"
	if len(h.action) > 0 {
		h.serveAction(w, req)
		return
	}

	if len(h.name) == 0 {
		loc.Path = fmt.Sprintf("%s/operator-lifecycle-manager/subscriptions", prefix)
	} else {
		loc.Path = fmt.Sprintf("%s/operator-lifecycle-manager/subscriptions/%s", prefix, h.name)
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

func (h *OLMProxyHandler) serveAction(w http.ResponseWriter, req *http.Request) {
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

var olmGVR = schema.GroupVersionResource{Group: "operators.coreos.com", Version: "v1alpha1", Resource: "subscriptions"}

// Get retrieves the object from the storage. It is required to support Patch.
func (h *OLMProxyHandler) getEventList(ctx context.Context) (*corev1.EventList, error) {
	return getEvents(ctx, h.cluster, h.clusterCredential, hpcResource, "Subscription", h.namespace, h.name)
}
