/*
Copyright 2024 the Unikorn Authors.

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

package cluster

import (
	"context"
	goerrors "errors"
	"fmt"
	"net/http"
	"slices"

	unikornv1 "github.com/unikorn-cloud/baremetal/pkg/apis/unikorn/v1alpha1"
	"github.com/unikorn-cloud/baremetal/pkg/openapi"
	unikornv1core "github.com/unikorn-cloud/core/pkg/apis/unikorn/v1alpha1"
	coreopenapi "github.com/unikorn-cloud/core/pkg/openapi"
	"github.com/unikorn-cloud/core/pkg/server/conversion"
	"github.com/unikorn-cloud/core/pkg/server/errors"
	"github.com/unikorn-cloud/core/pkg/util"
	"github.com/unikorn-cloud/identity/pkg/middleware/authorization"
	regionapi "github.com/unikorn-cloud/region/pkg/openapi"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	ErrResourceLookup = goerrors.New("could not find the requested resource")
)

// generator wraps up the myriad things we need to pass around as an object
// rather than a whole bunch of arguments.
type generator struct {
	// client allows Baremetal access.
	client client.Client
	// options allows access to resource defaults.
	options *Options
	// region is a client to access regions.
	region regionapi.ClientWithResponsesInterface
	// namespace the resource is provisioned in.
	namespace string
	// organizationID is the unique organization identifier.
	organizationID string
	// projectID is the unique project identifier.
	projectID string
}

func newGenerator(client client.Client, options *Options, region regionapi.ClientWithResponsesInterface, namespace, organizationID, projectID string) *generator {
	return &generator{
		client:         client,
		options:        options,
		region:         region,
		namespace:      namespace,
		organizationID: organizationID,
		projectID:      projectID,
	}
}

// convertMachine converts from a custom resource into the API definition.
func convertMachine(in *unikornv1.MachineGeneric) *openapi.MachinePool {
	machine := &openapi.MachinePool{
		Replicas: in.Replicas,
		FlavorId: in.FlavorID,
	}

	return machine
}

// convertWorkloadPool converts from a custom resource into the API definition.
func convertWorkloadPool(in *unikornv1.BaremetalClusterWorkloadPoolsPoolSpec) openapi.BaremetalClusterWorkloadPool {
	workloadPool := openapi.BaremetalClusterWorkloadPool{
		Name:    in.Name,
		Machine: *convertMachine(&in.BaremetalWorkloadPoolSpec.MachineGeneric),
	}

	return workloadPool
}

// convertWorkloadPools converts from a custom resource into the API definition.
func convertWorkloadPools(in *unikornv1.BaremetalCluster) []openapi.BaremetalClusterWorkloadPool {
	workloadPools := make([]openapi.BaremetalClusterWorkloadPool, len(in.Spec.WorkloadPools.Pools))

	for i := range in.Spec.WorkloadPools.Pools {
		workloadPools[i] = convertWorkloadPool(&in.Spec.WorkloadPools.Pools[i])
	}

	return workloadPools
}

// convert converts from a custom resource into the API definition.
func convert(in *unikornv1.BaremetalCluster) *openapi.BaremetalClusterRead {
	provisioningStatus := coreopenapi.ResourceProvisioningStatusUnknown

	if condition, err := in.StatusConditionRead(unikornv1core.ConditionAvailable); err == nil {
		provisioningStatus = conversion.ConvertStatusCondition(condition)
	}

	out := &openapi.BaremetalClusterRead{
		Metadata: conversion.ProjectScopedResourceReadMetadata(in, provisioningStatus),
		Spec: openapi.BaremetalClusterSpec{
			RegionId:      in.Spec.RegionID,
			WorkloadPools: convertWorkloadPools(in),
		},
	}

	return out
}

// uconvertList converts from a custom resource list into the API definition.
func convertList(in *unikornv1.BaremetalClusterList) openapi.BaremetalClusters {
	out := make(openapi.BaremetalClusters, len(in.Items))

	for i := range in.Items {
		out[i] = *convert(&in.Items[i])
	}

	return out
}

// defaultImage returns a default image for either control planes or workload pools
// based on the specified Baremetal version.
func (g *generator) defaultImage(ctx context.Context, request *openapi.BaremetalClusterWrite) (*regionapi.Image, error) {
	resp, err := g.region.GetApiV1OrganizationsOrganizationIDRegionsRegionIDImagesWithResponse(ctx, g.organizationID, request.Spec.RegionId)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode() != http.StatusOK {
		return nil, errors.OAuth2ServerError("failed to list images")
	}

	images := *resp.JSON200

	if len(images) == 0 {
		return nil, errors.OAuth2ServerError("unable to select an image")
	}

	return &images[0], nil
}

// generateNetwork generates the network part of a cluster.
func (g *generator) generateNetwork() *unikornv1.BaremetalClusterNetworkSpec {
	// Grab some defaults (as these are in the right format already)
	// the override with anything coming in from the API, if set.
	nodeNetwork := g.options.NodeNetwork
	dnsNameservers := g.options.DNSNameservers

	network := &unikornv1.BaremetalClusterNetworkSpec{
		NodeNetwork:    &unikornv1core.IPv4Prefix{IPNet: nodeNetwork},
		DNSNameservers: unikornv1core.IPv4AddressSliceFromIPSlice(dnsNameservers),
	}

	return network
}

// generateMachineGeneric generates a generic machine part of the cluster.
func (g *generator) generateMachineGeneric(ctx context.Context, request *openapi.BaremetalClusterWrite, m *openapi.MachinePool, flavor *regionapi.Flavor) (*unikornv1.MachineGeneric, error) {
	if m.Replicas == nil {
		m.Replicas = util.ToPointer(3)
	}

	image, err := g.defaultImage(ctx, request)
	if err != nil {
		return nil, err
	}

	machine := &unikornv1.MachineGeneric{
		Replicas: m.Replicas,
		ImageID:  util.ToPointer(image.Metadata.Id),
		FlavorID: &flavor.Metadata.Id,
	}

	return machine, nil
}

// generateWorkloadPools generates the workload pools part of a cluster.
func (g *generator) generateWorkloadPools(ctx context.Context, request *openapi.BaremetalClusterWrite) (*unikornv1.BaremetalClusterWorkloadPoolsSpec, error) {
	workloadPools := &unikornv1.BaremetalClusterWorkloadPoolsSpec{}

	for i := range request.Spec.WorkloadPools {
		pool := &request.Spec.WorkloadPools[i]

		flavor, err := g.lookupFlavor(ctx, request, *pool.Machine.FlavorId)
		if err != nil {
			return nil, err
		}

		machine, err := g.generateMachineGeneric(ctx, request, &pool.Machine, flavor)
		if err != nil {
			return nil, err
		}

		workloadPool := unikornv1.BaremetalClusterWorkloadPoolsPoolSpec{
			BaremetalWorkloadPoolSpec: unikornv1.BaremetalWorkloadPoolSpec{
				Name:           pool.Name,
				MachineGeneric: *machine,
			},
		}

		workloadPools.Pools = append(workloadPools.Pools, workloadPool)
	}

	return workloadPools, nil
}

// lookupFlavor resolves the flavor from its name.
// NOTE: It looks like garbage performance, but the provider should be memoized...
func (g *generator) lookupFlavor(ctx context.Context, request *openapi.BaremetalClusterWrite, id string) (*regionapi.Flavor, error) {
	resp, err := g.region.GetApiV1OrganizationsOrganizationIDRegionsRegionIDFlavorsWithResponse(ctx, g.organizationID, request.Spec.RegionId)
	if err != nil {
		return nil, err
	}

	flavors := *resp.JSON200

	index := slices.IndexFunc(flavors, func(flavor regionapi.Flavor) bool {
		return flavor.Metadata.Id == id
	})

	if index < 0 {
		return nil, fmt.Errorf("%w: flavor %s", ErrResourceLookup, id)
	}

	return &flavors[index], nil
}

// generate generates the full cluster custom resource.
// TODO: there are a lot of parameters being passed about, we should make this
// a struct and pass them as a single blob.
func (g *generator) generate(ctx context.Context, request *openapi.BaremetalClusterWrite) (*unikornv1.BaremetalCluster, error) {
	baremetalWorkloadPools, err := g.generateWorkloadPools(ctx, request)
	if err != nil {
		return nil, err
	}

	userinfo, err := authorization.UserinfoFromContext(ctx)
	if err != nil {
		return nil, err
	}

	cluster := &unikornv1.BaremetalCluster{
		ObjectMeta: conversion.NewObjectMetadata(&request.Metadata, g.namespace, userinfo.Sub).WithOrganization(g.organizationID).WithProject(g.projectID).Get(),
		Spec: unikornv1.BaremetalClusterSpec{
			RegionID:      request.Spec.RegionId,
			Network:       g.generateNetwork(),
			WorkloadPools: baremetalWorkloadPools,
		},
	}

	return cluster, nil
}
