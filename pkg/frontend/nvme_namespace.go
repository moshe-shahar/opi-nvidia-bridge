// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2022-2023 Dell Inc, or its subsidiaries.
// Copyright (c) 2022 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// Copyright (C) 2023 Intel Corporation

// Package frontend implememnts the FrontEnd APIs (host facing) of the storage Server
package frontend

import (
	"context"
	"fmt"
	"log"
	"path"
	"sort"
	"strconv"

	pb "github.com/opiproject/opi-api/storage/v1alpha1/gen/go"
	"github.com/opiproject/opi-nvidia-bridge/pkg/models"
	"github.com/opiproject/opi-spdk-bridge/pkg/server"

	"github.com/google/uuid"
	"go.einride.tech/aip/fieldbehavior"
	"go.einride.tech/aip/fieldmask"
	"go.einride.tech/aip/resourceid"
	"go.einride.tech/aip/resourcename"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func sortNvmeNamespaces(namespaces []*pb.NvmeNamespace) {
	sort.Slice(namespaces, func(i int, j int) bool {
		return namespaces[i].Spec.HostNsid < namespaces[j].Spec.HostNsid
	})
}

// CreateNvmeNamespace creates an Nvme namespace
func (s *Server) CreateNvmeNamespace(_ context.Context, in *pb.CreateNvmeNamespaceRequest) (*pb.NvmeNamespace, error) {
	log.Printf("CreateNvmeNamespace: Received from client: %v", in)
	// check required fields
	if err := fieldbehavior.ValidateRequiredFields(in); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// check input parameters validity
	if in.NvmeNamespace.Spec == nil || in.NvmeNamespace.Spec.SubsystemId == nil || in.NvmeNamespace.Spec.SubsystemId.Value == "" {
		return nil, status.Error(codes.InvalidArgument, "invalid input subsystem parameters")
	}
	// see https://google.aip.dev/133#user-specified-ids
	resourceID := resourceid.NewSystemGenerated()
	if in.NvmeNamespaceId != "" {
		err := resourceid.ValidateUserSettable(in.NvmeNamespaceId)
		if err != nil {
			log.Printf("error: %v", err)
			return nil, err
		}
		log.Printf("client provided the ID of a resource %v, ignoring the name field %v", in.NvmeNamespaceId, in.NvmeNamespace.Name)
		resourceID = in.NvmeNamespaceId
	}
	in.NvmeNamespace.Name = server.ResourceIDToVolumeName(resourceID)
	// idempotent API when called with same key, should return same object
	namespace, ok := s.Namespaces[in.NvmeNamespace.Name]
	if ok {
		log.Printf("Already existing NvmeNamespace with id %v", in.NvmeNamespace.Name)
		return namespace, nil
	}
	// not found, so create a new one
	subsys, ok := s.Subsystems[in.NvmeNamespace.Spec.SubsystemId.Value]
	if !ok {
		err := status.Errorf(codes.NotFound, "unable to find key %s", in.NvmeNamespace.Spec.SubsystemId.Value)
		log.Printf("error: %v", err)
		return nil, err
	}
	// TODO: do lookup through VolumeId key instead of using it's value
	params := models.NvdaControllerNvmeNamespaceAttachParams{
		BdevType: "spdk",
		Bdev:     in.NvmeNamespace.Spec.VolumeId.Value,
		Nsid:     int(in.NvmeNamespace.Spec.HostNsid),
		Subnqn:   subsys.Spec.Nqn,
		Cntlid:   0,
		UUID:     in.NvmeNamespace.Spec.Uuid.Value,
		Nguid:    in.NvmeNamespace.Spec.Nguid,
		Eui64:    strconv.FormatInt(in.NvmeNamespace.Spec.Eui64, 10),
	}
	var result models.NvdaControllerNvmeNamespaceAttachResult
	err := s.rpc.Call("controller_nvme_namespace_attach", &params, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)
	if !result {
		msg := fmt.Sprintf("Could not create NS: %s", in.NvmeNamespace.Name)
		log.Print(msg)
		return nil, status.Errorf(codes.InvalidArgument, msg)
	}
	response := server.ProtoClone(in.NvmeNamespace)
	response.Status = &pb.NvmeNamespaceStatus{PciState: 2, PciOperState: 1}
	s.Namespaces[in.NvmeNamespace.Name] = response
	return response, nil
}

// DeleteNvmeNamespace deletes an Nvme namespace
func (s *Server) DeleteNvmeNamespace(_ context.Context, in *pb.DeleteNvmeNamespaceRequest) (*emptypb.Empty, error) {
	log.Printf("DeleteNvmeNamespace: Received from client: %v", in)
	// check required fields
	if err := fieldbehavior.ValidateRequiredFields(in); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// Validate that a resource name conforms to the restrictions outlined in AIP-122.
	if err := resourcename.Validate(in.Name); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// fetch object from the database
	namespace, ok := s.Namespaces[in.Name]
	if !ok {
		if in.AllowMissing {
			return &emptypb.Empty{}, nil
		}
		err := status.Errorf(codes.NotFound, "unable to find key %s", in.Name)
		log.Printf("error: %v", err)
		return nil, err
	}
	subsys, ok := s.Subsystems[namespace.Spec.SubsystemId.Value]
	if !ok {
		err := fmt.Errorf("unable to find subsystem %s", namespace.Spec.SubsystemId.Value)
		log.Printf("error: %v", err)
		return nil, err
	}

	// TODO: fix hard-coded Cntlid
	params := models.NvdaControllerNvmeNamespaceDetachParams{
		Nsid:   int(namespace.Spec.HostNsid),
		Subnqn: subsys.Spec.Nqn,
		Cntlid: 0,
	}
	var result models.NvdaControllerNvmeNamespaceDetachResult
	err := s.rpc.Call("controller_nvme_namespace_detach", &params, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)
	if !result {
		msg := fmt.Sprintf("Could not delete NS: %s", namespace.Spec.SubsystemId.Value)
		log.Print(msg)
		return nil, status.Errorf(codes.InvalidArgument, msg)
	}
	delete(s.Namespaces, namespace.Name)
	return &emptypb.Empty{}, nil
}

// UpdateNvmeNamespace updates an Nvme namespace
func (s *Server) UpdateNvmeNamespace(_ context.Context, in *pb.UpdateNvmeNamespaceRequest) (*pb.NvmeNamespace, error) {
	log.Printf("UpdateNvmeNamespace: Received from client: %v", in)
	// check required fields
	if err := fieldbehavior.ValidateRequiredFields(in); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// Validate that a resource name conforms to the restrictions outlined in AIP-122.
	if err := resourcename.Validate(in.NvmeNamespace.Name); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// fetch object from the database
	volume, ok := s.Namespaces[in.NvmeNamespace.Name]
	if !ok {
		if in.AllowMissing {
			log.Printf("TODO: in case of AllowMissing, create a new resource, don;t return error")
		}
		err := status.Errorf(codes.NotFound, "unable to find key %s", in.NvmeNamespace.Name)
		log.Printf("error: %v", err)
		return nil, err
	}
	resourceID := path.Base(volume.Name)
	// update_mask = 2
	if err := fieldmask.Validate(in.UpdateMask, in.NvmeNamespace); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("TODO: use resourceID=%v", resourceID)
	return nil, status.Errorf(codes.Unimplemented, "UpdateNvmeNamespace method is not implemented")
}

// ListNvmeNamespaces lists Nvme namespaces
func (s *Server) ListNvmeNamespaces(_ context.Context, in *pb.ListNvmeNamespacesRequest) (*pb.ListNvmeNamespacesResponse, error) {
	log.Printf("ListNvmeNamespaces: Received from client: %v", in)
	// check required fields
	if err := fieldbehavior.ValidateRequiredFields(in); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// fetch object from the database
	size, offset, perr := server.ExtractPagination(in.PageSize, in.PageToken, s.Pagination)
	if perr != nil {
		log.Printf("error: %v", perr)
		return nil, perr
	}
	subsys, ok := s.Subsystems[in.Parent]
	if !ok {
		err := status.Errorf(codes.NotFound, "unable to find key %s", in.Parent)
		log.Printf("error: %v", err)
		return nil, err
	}
	// TODO: fix hard-coded Cntlid
	params := models.NvdaControllerNvmeNamespaceListParams{
		Subnqn: subsys.Spec.Nqn,
		Cntlid: 0,
	}
	var result models.NvdaControllerNvmeNamespaceListResult
	err := s.rpc.Call("controller_nvme_namespace_list", &params, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)
	token, hasMoreElements := "", false
	log.Printf("Limiting result len(%d) to [%d:%d]", len(result.Namespaces), offset, size)
	result.Namespaces, hasMoreElements = server.LimitPagination(result.Namespaces, offset, size)
	if hasMoreElements {
		token = uuid.New().String()
		s.Pagination[token] = offset + size
	}
	Blobarray := make([]*pb.NvmeNamespace, len(result.Namespaces))
	for i := range result.Namespaces {
		r := &result.Namespaces[i]
		Blobarray[i] = &pb.NvmeNamespace{Spec: &pb.NvmeNamespaceSpec{HostNsid: int32(r.Nsid)}}
	}
	sortNvmeNamespaces(Blobarray)
	return &pb.ListNvmeNamespacesResponse{NvmeNamespaces: Blobarray}, nil
}

// GetNvmeNamespace gets an Nvme namespace
func (s *Server) GetNvmeNamespace(_ context.Context, in *pb.GetNvmeNamespaceRequest) (*pb.NvmeNamespace, error) {
	log.Printf("GetNvmeNamespace: Received from client: %v", in)
	// check required fields
	if err := fieldbehavior.ValidateRequiredFields(in); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// Validate that a resource name conforms to the restrictions outlined in AIP-122.
	if err := resourcename.Validate(in.Name); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// fetch object from the database
	namespace, ok := s.Namespaces[in.Name]
	if !ok {
		err := status.Errorf(codes.NotFound, "unable to find key %s", in.Name)
		log.Printf("error: %v", err)
		return nil, err
	}
	subsys, ok := s.Subsystems[namespace.Spec.SubsystemId.Value]
	if !ok {
		err := status.Errorf(codes.NotFound, "unable to find key %s", namespace.Spec.SubsystemId.Value)
		log.Printf("error: %v", err)
		return nil, err
	}
	// TODO: fix hard-coded Cntlid
	params := models.NvdaControllerNvmeNamespaceListParams{
		Subnqn: subsys.Spec.Nqn,
		Cntlid: 0,
	}
	var result models.NvdaControllerNvmeNamespaceListResult
	err := s.rpc.Call("controller_nvme_namespace_list", &params, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)
	for i := range result.Namespaces {
		r := &result.Namespaces[i]
		if r.Nsid == int(namespace.Spec.HostNsid) {
			return &pb.NvmeNamespace{Spec: &pb.NvmeNamespaceSpec{HostNsid: int32(r.Nsid)}, Status: &pb.NvmeNamespaceStatus{PciState: 2, PciOperState: 1}}, nil
		}
	}
	msg := fmt.Sprintf("Could not find HostNsid: %d", namespace.Spec.HostNsid)
	log.Print(msg)
	return nil, status.Errorf(codes.InvalidArgument, msg)
}

// NvmeNamespaceStats gets an Nvme namespace stats
func (s *Server) NvmeNamespaceStats(_ context.Context, in *pb.NvmeNamespaceStatsRequest) (*pb.NvmeNamespaceStatsResponse, error) {
	log.Printf("NvmeNamespaceStats: Received from client: %v", in)
	// check required fields
	if err := fieldbehavior.ValidateRequiredFields(in); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// Validate that a resource name conforms to the restrictions outlined in AIP-122.
	if err := resourcename.Validate(in.NamespaceId.Value); err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	// fetch object from the database
	namespace, ok := s.Namespaces[in.NamespaceId.Value]
	if !ok {
		err := status.Errorf(codes.NotFound, "unable to find key %s", in.NamespaceId.Value)
		log.Printf("error: %v", err)
		return nil, err
	}
	var result models.NvdaControllerNvmeStatsResult
	err := s.rpc.Call("controller_nvme_get_iostat", nil, &result)
	if err != nil {
		log.Printf("error: %v", err)
		return nil, err
	}
	log.Printf("Received from SPDK: %v", result)
	for _, c := range result.Controllers {
		for _, r := range c.Bdevs {
			if r.BdevName == namespace.Spec.VolumeId.Value {
				return &pb.NvmeNamespaceStatsResponse{Id: in.NamespaceId, Stats: &pb.VolumeStats{
					ReadOpsCount:  int32(r.ReadIos),
					WriteOpsCount: int32(r.WriteIos),
				}}, nil
			}
		}
	}
	msg := fmt.Sprintf("Could not find BdevName: %s", namespace.Spec.VolumeId.Value)
	log.Print(msg)
	return nil, status.Errorf(codes.InvalidArgument, msg)
}