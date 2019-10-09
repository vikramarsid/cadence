// Copyright (c) 2019 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package history

import (
	ctx "context"

	"github.com/pborman/uuid"

	"github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/cache"
	"github.com/uber/cadence/common/cluster"
	"github.com/uber/cadence/common/collection"
	"github.com/uber/cadence/common/definition"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/log/tag"
	"github.com/uber/cadence/common/persistence"
)

type (
	workflowResetorImpl2 struct {
		shard           ShardContext
		domainCache     cache.DomainCache
		clusterMetadata cluster.Metadata
		historyV2Mgr    persistence.HistoryV2Manager
		historyCache    *historyCache
		transactionMgr  nDCTransactionMgr
		logger          log.Logger
	}
)

func newWorkflowResetor2(
	shard ShardContext,
	historyCache *historyCache,
	transactionMgr nDCTransactionMgr,
	logger log.Logger,
) *workflowResetorImpl2 {
	return &workflowResetorImpl2{
		shard:           shard,
		domainCache:     shard.GetDomainCache(),
		clusterMetadata: shard.GetClusterMetadata(),
		historyV2Mgr:    shard.GetHistoryV2Manager(),
		historyCache:    historyCache,
		transactionMgr:  transactionMgr,
		logger:          logger,
	}
}

// ResetWorkflowExecution only allows resetting to decisionTaskCompleted, but exclude that batch of decisionTaskCompleted/decisionTaskFailed/decisionTaskTimeout.
// It will then fail the decision with cause of "reset_workflow"
func (r *workflowResetorImpl2) ResetWorkflowExecution(
	ctx ctx.Context,
	domainName string,
	workflowID string,
	baseRunID string,
	baseResetUntilEventID int64,
	terminateReason string,
	resetReason string,
	additionalReapplyEvents []*shared.HistoryEvent,
) (resetRunID string, retError error) {

	domainID, err := r.domainCache.GetDomainID(domainName)
	if err != nil {
		return "", err
	}
	resetRunID = uuid.New()
	resetRequestID := uuid.New()

	baseWorkflow, err := r.transactionMgr.loadNDCWorkflow(
		ctx,
		domainID,
		workflowID,
		baseRunID,
	)
	if err != nil {
		return "", err
	}
	defer func() { baseWorkflow.getReleaseFn()(retError) }()

	baseBranchToken, err := baseWorkflow.getMutableState().GetCurrentBranchToken()
	if err != nil {
		return "", err
	}
	baseNextEventID := baseWorkflow.getMutableState().GetNextEventID()

	var currentWorkflow nDCWorkflow
	currentWorkflowTerminated := false
	var resetWorkflowVersion *int64

	currentRunID, err := r.transactionMgr.getCurrentWorkflowRunID(ctx, domainID, workflowID)
	if err != nil {
		return "", err
	} else if currentRunID == "" {
		return "", &shared.InternalServiceError{Message: "workflowResetorImpl2 encounter missing current workflow."}
	}

	if baseRunID == currentRunID {
		currentWorkflow = baseWorkflow
		domainEntry, err := r.domainCache.GetDomain(domainName)
		if err != nil {
			return "", err
		}
		resetWorkflowVersion = common.Int64Ptr(domainEntry.GetFailoverVersion())
	} else {
		currentWorkflow, err = r.transactionMgr.loadNDCWorkflow(
			ctx,
			domainID,
			workflowID,
			currentRunID,
		)
		if err != nil {
			return "", err
		}
		defer func() { currentWorkflow.getReleaseFn()(retError) }()

		currentMutableState := currentWorkflow.getMutableState()
		if currentMutableState.IsWorkflowExecutionRunning() {
			currentWorkflowTerminated = true
			if err := r.terminateWorkflow(currentMutableState, terminateReason); err != nil {
				return "", err
			}
			resetWorkflowVersion = common.Int64Ptr(currentMutableState.GetCurrentVersion())
		} else {
			domainEntry, err := r.domainCache.GetDomain(domainName)
			if err != nil {
				return "", err
			}
			resetWorkflowVersion = common.Int64Ptr(domainEntry.GetFailoverVersion())
		}
	}

	resetBranchToken, err := r.generateBranchToken(
		domainID,
		workflowID,
		baseBranchToken,
		baseResetUntilEventID,
		resetRunID,
	)
	if err != nil {
		return "", err
	}

	stateRebuilder := newNDCStateRebuilder(r.shard, r.logger)
	resetContext := newWorkflowExecutionContext(
		domainID,
		shared.WorkflowExecution{
			WorkflowId: common.StringPtr(workflowID),
			RunId:      common.StringPtr(resetRunID),
		},
		r.shard,
		r.shard.GetExecutionManager(),
		r.logger,
	)
	resetMutableState, resetHistorySize, err := stateRebuilder.rebuild(
		ctx,
		r.shard.GetTimeSource().Now(),
		definition.NewWorkflowIdentifier(
			domainID,
			workflowID,
			baseRunID,
		),
		baseBranchToken,
		baseResetUntilEventID,
		definition.NewWorkflowIdentifier(
			domainID,
			workflowID,
			resetRunID,
		),
		resetBranchToken,
		resetRequestID,
	)
	if err != nil {
		return "", err
	}

	resetContext.setHistorySize(resetHistorySize)

	baseLastEventVersion := resetMutableState.GetCurrentVersion()
	if baseLastEventVersion > *resetWorkflowVersion {
		return "", &shared.InternalServiceError{
			Message: "workflowResetorImpl2 encounter version mismatch.",
		}
	}
	if err := resetMutableState.UpdateCurrentVersion(*resetWorkflowVersion, false); err != nil {
		return "", err
	}

	decision, ok := resetMutableState.GetInFlightDecision()
	if !ok {
		return "", &shared.InternalServiceError{
			Message: "workflowResetorImpl2 encounter missing inflight decision.",
		}
	}

	_, err = resetMutableState.AddDecisionTaskFailedEvent(
		decision.ScheduleID,
		decision.StartedID, shared.DecisionTaskFailedCauseResetWorkflow,
		nil,
		identityHistoryService,
		resetReason,
		baseRunID,
		resetRunID,
		baseLastEventVersion,
	)
	if err != nil {
		return "", err
	}

	if err := r.failInflightActivity(resetMutableState, terminateReason); err != nil {
		return "", err
	}

	if err := r.reapplyContinueAsNewWorkflowEvents(
		ctx,
		resetMutableState,
		domainID,
		workflowID,
		baseRunID,
		baseBranchToken,
		baseResetUntilEventID,
		baseNextEventID,
	); err != nil {
		return "", err
	}

	if err := r.reapplyEvents(resetMutableState, additionalReapplyEvents); err != nil {
		return "", err
	}

	if err := scheduleDecision(resetMutableState); err != nil {
		return "", err
	}

	if !currentWorkflowTerminated {
		currentMutableState := currentWorkflow.getMutableState()
		currentRunID := currentMutableState.GetExecutionInfo().RunID
		currentLastWriteVersion, err := currentMutableState.GetLastWriteVersion()
		if err != nil {
			return "", err
		}

		now := r.shard.GetTimeSource().Now()
		resetWorkflowSnapshot, resetWorkflowEventsSeq, err := resetMutableState.CloseTransactionAsSnapshot(
			now,
			transactionPolicyActive,
		)
		if err != nil {
			return "", err
		}
		resetHistorySize, err := resetContext.persistFirstWorkflowEvents(resetWorkflowEventsSeq[0])
		if err != nil {
			return "", err
		}

		if err := resetContext.createWorkflowExecution(
			resetWorkflowSnapshot,
			resetHistorySize,
			now,
			persistence.CreateWorkflowModeContinueAsNew,
			currentRunID,
			currentLastWriteVersion,
		); err != nil {
			return "", err
		}
	} else {
		if err := currentWorkflow.getContext().updateWorkflowExecutionWithNewAsActive(
			r.shard.GetTimeSource().Now(),
			resetContext,
			resetMutableState,
		); err != nil {
			return "", err
		}
	}
	return resetRunID, err
}

func (r *workflowResetorImpl2) failInflightActivity(
	mutableState mutableState,
	terminateReason string,
) error {

	for _, ai := range mutableState.GetPendingActivityInfos() {
		if ai.StartedID != common.EmptyEventID {
			if _, err := mutableState.AddActivityTaskFailedEvent(
				ai.ScheduleID,
				ai.StartedID,
				&shared.RespondActivityTaskFailedRequest{
					Reason:   common.StringPtr(terminateReason),
					Details:  ai.Details,
					Identity: common.StringPtr(ai.StartedIdentity),
				},
			); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *workflowResetorImpl2) generateBranchToken(
	domainID string,
	workflowID string,
	forkBranchToken []byte,
	forkNodeID int64,
	resetRunID string,
) ([]byte, error) {
	// fork a new history branch
	shardID := r.shard.GetShardID()
	resp, err := r.shard.GetHistoryV2Manager().ForkHistoryBranch(&persistence.ForkHistoryBranchRequest{
		ForkBranchToken: forkBranchToken,
		ForkNodeID:      forkNodeID,
		Info:            persistence.BuildHistoryGarbageCleanupInfo(domainID, workflowID, resetRunID),
		ShardID:         common.IntPtr(shardID),
	})
	if err != nil {
		return nil, err
	}

	resetBranchToken := resp.NewBranchToken

	if errComplete := r.historyV2Mgr.CompleteForkBranch(&persistence.CompleteForkBranchRequest{
		BranchToken: resetBranchToken,
		Success:     true, // past lessons learnt from Cassandra & gocql tells that we cannot possibly find all timeout errors
		ShardID:     common.IntPtr(shardID),
	}); errComplete != nil {
		r.logger.WithTags(
			tag.Error(errComplete),
		).Error("workflowResetorImpl2 unable to complete creation of new branch.")
	}

	return resetBranchToken, nil
}

func (r *workflowResetorImpl2) terminateWorkflow(
	mutableState mutableState,
	terminateReason string,
) error {

	if decision, ok := mutableState.GetInFlightDecision(); ok {
		if err := failDecision(
			mutableState,
			decision,
			shared.DecisionTaskFailedCauseForceCloseDecision,
		); err != nil {
			return err
		}
	}

	_, err := mutableState.AddWorkflowExecutionTerminatedEvent(
		terminateReason,
		nil,
		identityHistoryService,
	)
	return err
}

func (r *workflowResetorImpl2) reapplyContinueAsNewWorkflowEvents(
	ctx ctx.Context,
	resetMutableState mutableState,
	domainID string,
	workflowID string,
	baseRunID string,
	baseBranchToken []byte,
	baseResetUntilEventID int64,
	baseNextEventID int64,
) error {

	// TODO change this logic to fetching all workflow [baseWorkflow, currentWorkflow]
	//  from visibility for better coverage of events eligible for re-application.

	var nextRunID string
	var err error

	// first special handling the remaining events for base workflow
	if nextRunID, err = r.reapplyWorkflowEvents(
		resetMutableState,
		definition.NewWorkflowIdentifier(domainID, workflowID, baseRunID),
		baseResetUntilEventID,
		baseNextEventID,
		baseBranchToken,
	); err != nil {
		return err
	}

	// second for remaining continue as new workflow, reapply eligible events
	for len(nextRunID) != 0 {

		nextWorkflow, err := r.transactionMgr.loadNDCWorkflow(ctx, domainID, workflowID, nextRunID)
		if err != nil {
			return err
		}
		nextWorkflowNextEventID := nextWorkflow.getMutableState().GetNextEventID()
		nextWorkflowBranchToken, err := nextWorkflow.getMutableState().GetCurrentBranchToken()
		if err != nil {
			return err
		}

		if nextRunID, err = r.reapplyWorkflowEvents(
			resetMutableState,
			definition.NewWorkflowIdentifier(domainID, workflowID, nextRunID),
			common.FirstEventID,
			nextWorkflowNextEventID,
			nextWorkflowBranchToken,
		); err != nil {
			return err
		}
	}
	return nil
}

func (r *workflowResetorImpl2) reapplyWorkflowEvents(
	mutableState mutableState,
	workflowIdentifier definition.WorkflowIdentifier,
	firstEventID int64,
	nextEventID int64,
	branchToken []byte,
) (string, error) {

	// TODO change this logic to fetching all workflow [baseWorkflow, currentWorkflow]
	//  from visibility for better coverage of events eligible for re-application.
	//  after the above change, this API do not have to return the continue as new run ID

	iter := collection.NewPagingIterator(r.getPaginationFn(
		workflowIdentifier,
		firstEventID,
		nextEventID,
		branchToken,
	))

	var nextRunID string
	var lastEvents []*shared.HistoryEvent

	for iter.HasNext() {
		batch, err := iter.Next()
		if err != nil {
			return "", err
		}
		lastEvents = batch.(*shared.History).Events
		if err := r.reapplyEvents(mutableState, lastEvents); err != nil {
			return "", err
		}
	}

	if len(lastEvents) > 0 {
		lastEvent := lastEvents[len(lastEvents)-1]
		if lastEvent.GetEventType() == shared.EventTypeWorkflowExecutionContinuedAsNew {
			nextRunID = lastEvent.GetWorkflowExecutionContinuedAsNewEventAttributes().GetNewExecutionRunId()
		}
	}
	return nextRunID, nil
}

func (r *workflowResetorImpl2) reapplyEvents(
	mutableState mutableState,
	events []*shared.HistoryEvent,
) error {

	for _, event := range events {
		switch event.GetEventType() {
		case shared.EventTypeWorkflowExecutionSignaled:
			attr := event.GetWorkflowExecutionSignaledEventAttributes()
			if _, err := mutableState.AddWorkflowExecutionSignaled(
				attr.GetSignalName(),
				attr.GetInput(),
				attr.GetIdentity(),
			); err != nil {
				return err
			}
		default:
			// events other than signal will be ignored
		}
	}
	return nil
}

func (r *workflowResetorImpl2) getPaginationFn(
	workflowIdentifier definition.WorkflowIdentifier,
	firstEventID int64,
	nextEventID int64,
	branchToken []byte,
) collection.PaginationFn {

	return func(paginationToken []byte) ([]interface{}, []byte, error) {

		_, historyBatches, token, _, err := PaginateHistory(
			nil,
			r.historyV2Mgr,
			true,
			workflowIdentifier.DomainID,
			workflowIdentifier.WorkflowID,
			workflowIdentifier.RunID,
			persistence.EventStoreVersionV2,
			branchToken,
			firstEventID,
			nextEventID,
			paginationToken,
			nDCDefaultPageSize,
			common.IntPtr(r.shard.GetShardID()),
		)
		if err != nil {
			return nil, nil, err
		}

		var paginateItems []interface{}
		for _, history := range historyBatches {
			paginateItems = append(paginateItems, history)
		}
		return paginateItems, token, nil
	}
}