/*
Copyright 2014 Google Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package master

import (
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api/latest"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api/v1beta1"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api/v1beta2"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/apiserver"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/cloudprovider"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/registry/binding"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/registry/controller"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/registry/endpoint"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/registry/etcd"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/registry/event"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/registry/generic"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/registry/minion"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/registry/pod"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/registry/service"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/runtime"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/tools"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"

	"github.com/golang/glog"
)

// Config is a structure used to configure a Master.
type Config struct {
	Client             *client.Client
	Cloud              cloudprovider.Interface
	EtcdHelper         tools.EtcdHelper
	HealthCheckMinions bool
	Minions            []string
	MinionCacheTTL     time.Duration
	EventTTL           time.Duration
	MinionRegexp       string
	PodInfoGetter      client.PodInfoGetter
	NodeResources      api.NodeResources
}

// Master contains state for a Kubernetes cluster master/api server.
type Master struct {
	podRegistry        pod.Registry
	controllerRegistry controller.Registry
	serviceRegistry    service.Registry
	endpointRegistry   endpoint.Registry
	minionRegistry     minion.Registry
	bindingRegistry    binding.Registry
	eventRegistry      generic.Registry
	storage            map[string]apiserver.RESTStorage
	client             *client.Client
}

// NewEtcdHelper returns an EtcdHelper for the provided arguments or an error if the version
// is incorrect.
func NewEtcdHelper(client tools.EtcdGetSet, version string) (helper tools.EtcdHelper, err error) {
	if version == "" {
		version = latest.Version
	}
	versionInterfaces, err := latest.InterfacesFor(version)
	if err != nil {
		return helper, err
	}
	return tools.EtcdHelper{client, versionInterfaces.Codec, tools.RuntimeVersionAdapter{versionInterfaces.ResourceVersioner}}, nil
}

// New returns a new instance of Master connected to the given etcd server.
func New(c *Config) *Master {
	minionRegistry := makeMinionRegistry(c)
	serviceRegistry := etcd.NewRegistry(c.EtcdHelper, nil)
	manifestFactory := &pod.BasicManifestFactory{
		ServiceRegistry: serviceRegistry,
	}
	m := &Master{
		podRegistry:        etcd.NewRegistry(c.EtcdHelper, manifestFactory),
		controllerRegistry: etcd.NewRegistry(c.EtcdHelper, nil),
		serviceRegistry:    serviceRegistry,
		endpointRegistry:   etcd.NewRegistry(c.EtcdHelper, nil),
		bindingRegistry:    etcd.NewRegistry(c.EtcdHelper, manifestFactory),
		eventRegistry:      event.NewEtcdRegistry(c.EtcdHelper, uint64(c.EventTTL.Seconds())),
		minionRegistry:     minionRegistry,
		client:             c.Client,
	}
	m.init(c.Cloud, c.PodInfoGetter)
	return m
}

func makeMinionRegistry(c *Config) minion.Registry {
	var minionRegistry minion.Registry
	if c.Cloud != nil && len(c.MinionRegexp) > 0 {
		var err error
		minionRegistry, err = minion.NewCloudRegistry(c.Cloud, c.MinionRegexp, &c.NodeResources)
		if err != nil {
			glog.Errorf("Failed to initalize cloud minion registry reverting to static registry (%#v)", err)
		}
	}
	if minionRegistry == nil {
		minionRegistry = etcd.NewRegistry(c.EtcdHelper, nil)
		for _, minionID := range c.Minions {
			minionRegistry.CreateMinion(nil, &api.Minion{
				TypeMeta:      api.TypeMeta{ID: minionID},
				NodeResources: c.NodeResources,
			})
		}
	}
	if c.HealthCheckMinions {
		minionRegistry = minion.NewHealthyRegistry(minionRegistry, &http.Client{})
	}
	if c.MinionCacheTTL > 0 {
		cachingMinionRegistry, err := minion.NewCachingRegistry(minionRegistry, c.MinionCacheTTL)
		if err != nil {
			glog.Errorf("Failed to initialize caching layer, ignoring cache.")
		} else {
			minionRegistry = cachingMinionRegistry
		}
	}
	return minionRegistry
}

func (m *Master) init(cloud cloudprovider.Interface, podInfoGetter client.PodInfoGetter) {
	podCache := NewPodCache(podInfoGetter, m.podRegistry)
	go util.Forever(func() { podCache.UpdateAllContainers() }, time.Second*30)

	m.storage = map[string]apiserver.RESTStorage{
		"pods": pod.NewREST(&pod.RESTConfig{
			CloudProvider: cloud,
			PodCache:      podCache,
			PodInfoGetter: podInfoGetter,
			Registry:      m.podRegistry,
			Minions:       m.client,
		}),
		"replicationControllers": controller.NewREST(m.controllerRegistry, m.podRegistry),
		"services":               service.NewREST(m.serviceRegistry, cloud, m.minionRegistry),
		"endpoints":              endpoint.NewREST(m.endpointRegistry),
		"minions":                minion.NewREST(m.minionRegistry),
		"events":                 event.NewREST(m.eventRegistry),

		// TODO: should appear only in scheduler API group.
		"bindings": binding.NewREST(m.bindingRegistry),
	}
}

// API_v1beta1 returns the resources and codec for API version v1beta1.
func (m *Master) API_v1beta1() (map[string]apiserver.RESTStorage, runtime.Codec, string, runtime.SelfLinker) {
	storage := make(map[string]apiserver.RESTStorage)
	for k, v := range m.storage {
		storage[k] = v
	}
	return storage, v1beta1.Codec, "/api/v1beta1", latest.SelfLinker
}

// API_v1beta2 returns the resources and codec for API version v1beta2.
func (m *Master) API_v1beta2() (map[string]apiserver.RESTStorage, runtime.Codec, string, runtime.SelfLinker) {
	storage := make(map[string]apiserver.RESTStorage)
	for k, v := range m.storage {
		storage[k] = v
	}
	return storage, v1beta2.Codec, "/api/v1beta1", latest.SelfLinker
}
