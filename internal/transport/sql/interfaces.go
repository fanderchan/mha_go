package sql

import (
	"context"

	"mha-go/internal/domain"
)

type SecretResolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

type Inspector interface {
	Inspect(ctx context.Context, node domain.NodeSpec) (*Inspection, error)
}

type Inspection struct {
	NodeID                     string
	Address                    string
	ServerUUID                 string
	Version                    string
	VersionComment             string
	VersionSeries              string
	GTIDMode                   string
	GTIDExecuted               string
	ReadOnly                   bool
	SuperReadOnly              bool
	SemiSyncSourceEnabled      bool
	SemiSyncSourceOperational  bool
	SemiSyncReplicaOperational bool
	ReplicaChannels            []ReplicaChannelStatus
}

type ReplicaChannelStatus struct {
	ChannelName         string
	SourceHost          string
	SourcePort          int
	SourceUUID          string
	AutoPosition        bool
	IOThreadRunning     bool
	SQLThreadRunning    bool
	RetrievedGTIDSet    string
	ExecutedGTIDSet     string
	SecondsBehindSource int64
	LastIOError         string
	LastSQLError        string
}
