/*
Copyright 2022.

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
// Code generated by lister-gen. DO NOT EDIT.

package v1

import (
	v1 "github.com/neondatabase/autoscaling/neonvm/apis/neonvm/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/listers"
	"k8s.io/client-go/tools/cache"
)

// VirtualMachineMigrationLister helps list VirtualMachineMigrations.
// All objects returned here must be treated as read-only.
type VirtualMachineMigrationLister interface {
	// List lists all VirtualMachineMigrations in the indexer.
	// Objects returned here must be treated as read-only.
	List(selector labels.Selector) (ret []*v1.VirtualMachineMigration, err error)
	// VirtualMachineMigrations returns an object that can list and get VirtualMachineMigrations.
	VirtualMachineMigrations(namespace string) VirtualMachineMigrationNamespaceLister
	VirtualMachineMigrationListerExpansion
}

// virtualMachineMigrationLister implements the VirtualMachineMigrationLister interface.
type virtualMachineMigrationLister struct {
	listers.ResourceIndexer[*v1.VirtualMachineMigration]
}

// NewVirtualMachineMigrationLister returns a new VirtualMachineMigrationLister.
func NewVirtualMachineMigrationLister(indexer cache.Indexer) VirtualMachineMigrationLister {
	return &virtualMachineMigrationLister{listers.New[*v1.VirtualMachineMigration](indexer, v1.Resource("virtualmachinemigration"))}
}

// VirtualMachineMigrations returns an object that can list and get VirtualMachineMigrations.
func (s *virtualMachineMigrationLister) VirtualMachineMigrations(namespace string) VirtualMachineMigrationNamespaceLister {
	return virtualMachineMigrationNamespaceLister{listers.NewNamespaced[*v1.VirtualMachineMigration](s.ResourceIndexer, namespace)}
}

// VirtualMachineMigrationNamespaceLister helps list and get VirtualMachineMigrations.
// All objects returned here must be treated as read-only.
type VirtualMachineMigrationNamespaceLister interface {
	// List lists all VirtualMachineMigrations in the indexer for a given namespace.
	// Objects returned here must be treated as read-only.
	List(selector labels.Selector) (ret []*v1.VirtualMachineMigration, err error)
	// Get retrieves the VirtualMachineMigration from the indexer for a given namespace and name.
	// Objects returned here must be treated as read-only.
	Get(name string) (*v1.VirtualMachineMigration, error)
	VirtualMachineMigrationNamespaceListerExpansion
}

// virtualMachineMigrationNamespaceLister implements the VirtualMachineMigrationNamespaceLister
// interface.
type virtualMachineMigrationNamespaceLister struct {
	listers.ResourceIndexer[*v1.VirtualMachineMigration]
}
