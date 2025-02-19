// Copyright 2022 CeresDB Project Authors. Licensed under Apache-2.0.

package coordinator

import (
	"context"

	"github.com/CeresDB/ceresdbproto/golang/pkg/metaservicepb"
	"github.com/CeresDB/ceresmeta/pkg/log"
	"github.com/CeresDB/ceresmeta/server/cluster"
	"github.com/CeresDB/ceresmeta/server/coordinator/eventdispatch"
	"github.com/CeresDB/ceresmeta/server/coordinator/procedure"
	"github.com/CeresDB/ceresmeta/server/coordinator/procedure/dml/createpartitiontable"
	"github.com/CeresDB/ceresmeta/server/coordinator/procedure/dml/createtable"
	"github.com/CeresDB/ceresmeta/server/coordinator/procedure/dml/droppartitiontable"
	"github.com/CeresDB/ceresmeta/server/coordinator/procedure/dml/droptable"
	"github.com/CeresDB/ceresmeta/server/coordinator/procedure/operation/scatter"
	"github.com/CeresDB/ceresmeta/server/coordinator/procedure/operation/split"
	"github.com/CeresDB/ceresmeta/server/coordinator/procedure/operation/transferleader"
	"github.com/CeresDB/ceresmeta/server/id"
	"github.com/CeresDB/ceresmeta/server/storage"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

const defaultPartitionTableNum = 1

type Factory struct {
	idAllocator    id.Allocator
	dispatch       eventdispatch.Dispatch
	storage        procedure.Storage
	clusterManager cluster.Manager
	shardPicker    ShardPicker

	// TODO: This is a temporary implementation version, which needs to be refined to the table level later.
	partitionTableProportionOfNodes float32
}

type ScatterRequest struct {
	Cluster  *cluster.Cluster
	ShardIDs []storage.ShardID
}

type CreateTableRequest struct {
	Cluster   *cluster.Cluster
	SourceReq *metaservicepb.CreateTableRequest

	OnSucceeded func(cluster.CreateTableResult) error
	OnFailed    func(error) error
}

func (request *CreateTableRequest) isPartitionTable() bool {
	return request.SourceReq.PartitionTableInfo != nil
}

type DropTableRequest struct {
	Cluster   *cluster.Cluster
	SourceReq *metaservicepb.DropTableRequest

	OnSucceeded func(cluster.TableInfo) error
	OnFailed    func(error) error
}

func (d DropTableRequest) IsPartitionTable() bool {
	return d.SourceReq.PartitionTableInfo != nil
}

type TransferLeaderRequest struct {
	ClusterName       string
	ShardID           storage.ShardID
	OldLeaderNodeName string
	NewLeaderNodeName string
	ClusterVersion    uint64
}

type SplitRequest struct {
	ClusterName    string
	SchemaName     string
	TableNames     []string
	ShardID        storage.ShardID
	NewShardID     storage.ShardID
	TargetNodeName string
	ClusterVersion uint64
}

type CreatePartitionTableRequest struct {
	Cluster   *cluster.Cluster
	SourceReq *metaservicepb.CreateTableRequest

	PartitionTableRatioOfNodes float32

	OnSucceeded func(cluster.CreateTableResult) error
	OnFailed    func(error) error
}

func NewFactory(allocator id.Allocator, dispatch eventdispatch.Dispatch, storage procedure.Storage, manager cluster.Manager, partitionTableProportionOfNodes float32) *Factory {
	return &Factory{
		idAllocator:                     allocator,
		dispatch:                        dispatch,
		storage:                         storage,
		clusterManager:                  manager,
		shardPicker:                     NewRandomBalancedShardPicker(manager),
		partitionTableProportionOfNodes: partitionTableProportionOfNodes,
	}
}

func (f *Factory) CreateScatterProcedure(ctx context.Context, request ScatterRequest) (procedure.Procedure, error) {
	id, err := f.allocProcedureID(ctx)
	if err != nil {
		return nil, err
	}
	procedure := scatter.NewProcedure(f.dispatch, request.Cluster, id, request.ShardIDs)
	return procedure, nil
}

func (f *Factory) MakeCreateTableProcedure(ctx context.Context, request CreateTableRequest) (procedure.Procedure, error) {
	isPartitionTable := request.isPartitionTable()

	if isPartitionTable {
		return f.makeCreatePartitionTableProcedure(ctx, CreatePartitionTableRequest{
			Cluster:                    request.Cluster,
			SourceReq:                  request.SourceReq,
			PartitionTableRatioOfNodes: f.partitionTableProportionOfNodes,
			OnSucceeded:                request.OnSucceeded,
			OnFailed:                   request.OnFailed,
		})
	}

	return f.makeCreateTableProcedure(ctx, request)
}

func (f *Factory) makeCreateTableProcedure(ctx context.Context, request CreateTableRequest) (procedure.Procedure, error) {
	id, err := f.allocProcedureID(ctx)
	if err != nil {
		return nil, err
	}
	shards, err := f.shardPicker.PickShards(ctx, request.Cluster.Name(), 1, false)
	if err != nil {
		log.Error("pick table shard", zap.Error(err))
		return nil, errors.WithMessage(err, "pick table shard")
	}
	if len(shards) != 1 {
		log.Error("pick table shards length not equal 1", zap.Int("shards", len(shards)))
		return nil, errors.WithMessagef(procedure.ErrPickShard, "pick table shard, shards length:%d", len(shards))
	}

	procedure := createtable.NewProcedure(createtable.ProcedureRequest{
		Dispatch:    f.dispatch,
		Cluster:     request.Cluster,
		ID:          id,
		ShardID:     shards[0].ShardInfo.ID,
		Req:         request.SourceReq,
		OnSucceeded: request.OnSucceeded,
		OnFailed:    request.OnFailed,
	})
	return procedure, nil
}

func (f *Factory) makeCreatePartitionTableProcedure(ctx context.Context, request CreatePartitionTableRequest) (procedure.Procedure, error) {
	id, err := f.allocProcedureID(ctx)
	if err != nil {
		return nil, err
	}

	getNodeShardResult, err := request.Cluster.GetNodeShards(ctx)
	if err != nil {
		log.Error("cluster get node shard result")
		return nil, err
	}

	nodeNames := make(map[string]int)
	for _, nodeShard := range getNodeShardResult.NodeShards {
		nodeNames[nodeShard.ShardNode.NodeName] = 1
	}

	partitionTableNum := procedure.Max(defaultPartitionTableNum, int(float32(len(nodeNames))*request.PartitionTableRatioOfNodes))

	partitionTableShards, err := f.shardPicker.PickShards(ctx, request.Cluster.Name(), partitionTableNum, false)
	if err != nil {
		return nil, errors.WithMessage(err, "pick partition table shards")
	}

	subTableShards, err := f.shardPicker.PickShards(ctx, request.Cluster.Name(), len(request.SourceReq.PartitionTableInfo.SubTableNames), true)
	if err != nil {
		return nil, errors.WithMessage(err, "pick data table shards")
	}

	procedure := createpartitiontable.NewProcedure(createpartitiontable.ProcedureRequest{
		ID:                   id,
		Cluster:              request.Cluster,
		Dispatch:             f.dispatch,
		Storage:              f.storage,
		Req:                  request.SourceReq,
		PartitionTableShards: partitionTableShards,
		SubTablesShards:      subTableShards,
		OnSucceeded:          request.OnSucceeded,
		OnFailed:             request.OnFailed,
	})
	return procedure, nil
}

func (f *Factory) CreateDropTableProcedure(ctx context.Context, request DropTableRequest) (procedure.Procedure, error) {
	id, err := f.allocProcedureID(ctx)
	if err != nil {
		return nil, err
	}

	if request.IsPartitionTable() {
		req := droppartitiontable.ProcedureRequest{
			ID:          id,
			Cluster:     request.Cluster,
			Dispatch:    f.dispatch,
			Storage:     f.storage,
			Request:     request.SourceReq,
			OnSucceeded: request.OnSucceeded,
			OnFailed:    request.OnFailed,
		}
		procedure := droppartitiontable.NewProcedure(req)
		return procedure, nil
	}

	procedure := droptable.NewDropTableProcedure(f.dispatch, request.Cluster, id,
		request.SourceReq, request.OnSucceeded, request.OnFailed)
	return procedure, nil
}

func (f *Factory) CreateTransferLeaderProcedure(ctx context.Context, request TransferLeaderRequest) (procedure.Procedure, error) {
	id, err := f.allocProcedureID(ctx)
	if err != nil {
		return nil, err
	}

	c, err := f.clusterManager.GetCluster(ctx, request.ClusterName)
	if err != nil {
		log.Error("cluster not found", zap.String("clusterName", request.ClusterName))
		return nil, cluster.ErrClusterNotFound
	}

	return transferleader.NewProcedure(f.dispatch, c, f.storage,
		request.ShardID, request.OldLeaderNodeName, request.NewLeaderNodeName, id)
}

func (f *Factory) CreateSplitProcedure(ctx context.Context, request SplitRequest) (procedure.Procedure, error) {
	id, err := f.allocProcedureID(ctx)
	if err != nil {
		return nil, err
	}

	c, err := f.clusterManager.GetCluster(ctx, request.ClusterName)
	if err != nil {
		log.Error("cluster not found", zap.String("clusterName", request.ClusterName))
		return nil, cluster.ErrClusterNotFound
	}

	procedure := split.NewProcedure(id, f.dispatch, f.storage, c, request.SchemaName, request.ShardID, request.NewShardID, request.TableNames, request.TargetNodeName)
	return procedure, nil
}

func (f *Factory) allocProcedureID(ctx context.Context) (uint64, error) {
	id, err := f.idAllocator.Alloc(ctx)
	if err != nil {
		return 0, errors.WithMessage(err, "alloc procedure id")
	}
	return id, nil
}
