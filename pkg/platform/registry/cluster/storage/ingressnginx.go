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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	netutil "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/apiserver/pkg/registry/rest"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
	platforminternalclient "tkestack.io/tke/api/client/clientset/internalversion/typed/platform/internalversion"
	"tkestack.io/tke/api/platform"
	"tkestack.io/tke/pkg/platform/util"
)

// NginxIngressREST implements proxy NginxIngress request to cluster of user.
type NginxIngressREST struct {
	rest.Storage
	store          *registry.Store
	platformClient platforminternalclient.PlatformInterface
}

// ConnectMethods returns the list of HTTP methods that can be proxied
func (r *NginxIngressREST) ConnectMethods() []string {
	return []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}
}

// NewConnectOptions returns versioned resource that represents proxy parameters
func (r *NginxIngressREST) NewConnectOptions() (runtime.Object, bool, string) {
	return &platform.NginxIngressProxyOptions{}, false, ""
}

// Connect returns a handler for the kube-apiserver proxy
func (r *NginxIngressREST) Connect(ctx context.Context, clusterName string, opts runtime.Object, responder rest.Responder) (http.Handler, error) {
	clusterObject, err := r.store.Get(ctx, clusterName, &metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	cluster := clusterObject.(*platform.Cluster)
	proxyOpts := opts.(*platform.NginxIngressProxyOptions)

	location, transport, token, err := util.APIServerLocationByCluster(ctx, cluster, r.platformClient)
	if err != nil {
		return nil, err
	}
	return &nginxIngressProxyHandler{
		location:  location,
		transport: transport,
		token:     token,
		name:      proxyOpts.Name,
	}, nil
}

// New creates a new NginxIngress proxy options object
func (r *NginxIngressREST) New() runtime.Object {
	return &platform.NginxIngressProxyOptions{}
}

type nginxIngressProxyHandler struct {
	transport http.RoundTripper
	location  *url.URL
	token     string
	name      string
}

func (h *nginxIngressProxyHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	loc := *h.location
	loc.RawQuery = req.URL.RawQuery

	prefix := "/apis/cloud.tencent.com/v1alpha1"

	if len(h.name) == 0 {
		loc.Path = fmt.Sprintf("%s/nginxingresses", prefix)
	} else {
		loc.Path = fmt.Sprintf("%s/nginxingresses/%s", prefix, h.name)
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
