// Copyright Project Contour Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package operator

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	operatorv1alpha1 "github.com/projectcontour/contour-operator/api/v1alpha1"
	"github.com/projectcontour/contour-operator/internal/config"
	"github.com/projectcontour/contour-operator/internal/controller"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	controller_runtime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	gatewayv1alpha1 "sigs.k8s.io/gateway-api/apis/v1alpha1"
)

const (
	operatorName = "contour_operator"
)

// Clients holds the API clients required by Operator.
type Client struct {
	client.Client
	meta.RESTMapper
}

// Operator is the scaffolding for the contour operator. It sets up dependencies
// and defines the topology of the operator and its managed components, wiring
// them together. Operator knows what specific resource types should produce
// operator events.
type Operator struct {
	client  Client
	manager manager.Manager
	log     logr.Logger
	config  *config.Config
}

// +kubebuilder:rbac:groups=operator.projectcontour.io,resources=contours,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=operator.projectcontour.io,resources=contours/status,verbs=get;update;patch
// cert-gen needs create/update secrets.
// +kubebuilder:rbac:groups="",resources=namespaces;secrets;serviceaccounts;services,verbs=get;list;watch;delete;create;update
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;delete;create;update
// +kubebuilder:rbac:groups="",resources=endpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=networking.x-k8s.io,resources=gatewayclasses;gateways;backendpolicies;httproutes;tlsroutes,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=networking.x-k8s.io,resources=gatewayclasses/status;gateways/status;backendpolicies/status;httproutes/status;tlsroutes/status,verbs=create;get;update
// Required for Contour to set "unsupported" status
// +kubebuilder:rbac:groups=networking.x-k8s.io,resources=udproutes;tcproutes,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.x-k8s.io,resources=udproutes/status;tcproutes/status,verbs=update
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses;ingressclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses/status,verbs=create;get;update
// +kubebuilder:rbac:groups=projectcontour.io,resources=httpproxies;tlscertificatedelegations;extensionservices;contourconfigurations,verbs=get;list;watch
// +kubebuilder:rbac:groups=projectcontour.io,resources=httpproxies/status;extensionservices/status;contourconfigurations/status,verbs=create;get;update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings;roles;rolebindings,verbs=get;list;delete;create;update;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;delete;create;update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;delete;create;update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;delete;create;update
// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;delete;create;update
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list

// New creates a new operator from cliCfg and opCfg.
func New(cliCfg *rest.Config, opCfg *config.Config) (*Operator, error) {
	nonCached := []client.Object{&operatorv1alpha1.Contour{}, &gatewayv1alpha1.GatewayClass{},
		&gatewayv1alpha1.Gateway{}, &apiextensionsv1.CustomResourceDefinition{}}
	mgrOpts := manager.Options{
		Scheme:                GetOperatorScheme(),
		LeaderElection:        opCfg.LeaderElection,
		LeaderElectionID:      opCfg.LeaderElectionID,
		MetricsBindAddress:    opCfg.MetricsBindAddress,
		ClientDisableCacheFor: nonCached,
	}
	mgr, err := controller_runtime.NewManager(cliCfg, mgrOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to create manager: %w", err)
	}

	// Create and register the contour controller with the operator manager.
	if _, err := controller.New(mgr, controller.Config{
		ContourImage: opCfg.ContourImage,
		EnvoyImage:   opCfg.EnvoyImage,
	}); err != nil {
		return nil, fmt.Errorf("failed to create contour controller: %w", err)
	}

	restMapper, err := apiutil.NewDiscoveryRESTMapper(cliCfg)
	if err != nil {
		return nil, err
	}

	return &Operator{
		manager: mgr,
		client:  Client{mgr.GetClient(), restMapper},
		log:     controller_runtime.Log.WithName(operatorName),
		config:  opCfg,
	}, nil
}

// Start creates Gateway API controllers (if configured) and starts the operator
// synchronously until a message is received from ctx.
func (o *Operator) Start(ctx context.Context) error {
	errChan := make(chan error)
	go func() {
		errChan <- o.manager.Start(ctx)
	}()

	// Wait for the manager to exit or an explicit stop.
	select {
	case <-ctx.Done():
		return nil
	case err := <-errChan:
		return err
	}
}

// GatewayAPIResources for Operator.
// The list omits TCP and UDP routes since they're unsupported by operator.
func GatewayAPIResources() []schema.GroupVersionResource {
	return []schema.GroupVersionResource{{
		Group:    gatewayv1alpha1.GroupVersion.Group,
		Version:  gatewayv1alpha1.GroupVersion.Version,
		Resource: "gatewayclasses",
	}, {
		Group:    gatewayv1alpha1.GroupVersion.Group,
		Version:  gatewayv1alpha1.GroupVersion.Version,
		Resource: "gateways",
	}, {
		Group:    gatewayv1alpha1.GroupVersion.Group,
		Version:  gatewayv1alpha1.GroupVersion.Version,
		Resource: "httproutes",
	}, {
		Group:    gatewayv1alpha1.GroupVersion.Group,
		Version:  gatewayv1alpha1.GroupVersion.Version,
		Resource: "tlsroutes",
	}}
}
