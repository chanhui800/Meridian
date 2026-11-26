package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNodeDeletionQueuesAuthenticatedAgentUninstallAndWaitsForAck(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	created, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "auto uninstall online", Address: "203.0.113.47", Port: 19443}, now)
	if err != nil {
		t.Fatal(err)
	}
	enrolled, agentToken, err := app.db.EnrollControlNode(enrollment, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if enrolled.ID != created.ID {
		t.Fatalf("enrolled node id=%d, created=%d", enrolled.ID, created.ID)
	}

	pending, queued, err := app.db.BeginNodeUninstall(created.ID, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !queued || !pending.UninstallPending || pending.Enabled || len(pending.uninstallRequestID) != 32 {
		t.Fatalf("uninstall was not queued durably: queued=%v node=%+v", queued, pending)
	}

	legacy, err := app.buildAgentConfigForRequest(context.Background(), agentToken, now.Add(3*time.Second), "", "v1.11.12", agentSessionRequest{ID: "legacy-delete-session", Epoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	if legacy.UninstallRequestID != "" || legacy.ConfigHash == "" {
		t.Fatalf("old Agent received an unsupported uninstall command: %+v", legacy)
	}

	command, err := app.buildAgentConfigForRequest(context.Background(), agentToken, now.Add(4*time.Second), "", "v1.11.12", agentSessionRequest{ID: "current-delete-session", Epoch: 2, Capabilities: agentSelfUninstallCapability})
	if err != nil {
		t.Fatal(err)
	}
	if command.UninstallRequestID != pending.uninstallRequestID || command.NodeGUID != pending.GUID || len(command.Routes) != 0 || command.ConfigHash != "" {
		t.Fatalf("Agent uninstall command was not isolated and node-bound: %+v", command)
	}

	if err := app.db.AcknowledgeNodeUninstall(agentToken, pending.GUID, "00000000000000000000000000000000"); !errors.Is(err, errInvalidAgentToken) {
		t.Fatalf("wrong uninstall request ID error=%v, want invalid Agent credential", err)
	}
	if err := app.db.AcknowledgeNodeUninstall(agentToken, "another-node-guid", pending.uninstallRequestID); !errors.Is(err, errInvalidAgentToken) {
		t.Fatalf("wrong node GUID error=%v, want invalid Agent credential", err)
	}
	if _, err := app.db.controlNodeByID(created.ID, now); err != nil {
		t.Fatalf("requesting deletion removed the node before Agent ACK: %v", err)
	}
	if err := app.db.AcknowledgeNodeUninstall(agentToken, pending.GUID, pending.uninstallRequestID); err != nil {
		t.Fatalf("valid Agent uninstall ACK failed: %v", err)
	}
	if _, err := app.db.controlNodeByID(created.ID, now); !errors.Is(err, errNodeNotFound) {
		t.Fatalf("node row remains after uninstall ACK: %v", err)
	}
}

func TestAgentSelfUninstallCommandRequiresMatchingNodeAndRequest(t *testing.T) {
	base := AgentRuntimeConfig{SchemaVersion: agentConfigSchemaVersion, NodeGUID: "node-guid", UninstallRequestID: "0123456789abcdef0123456789abcdef"}
	if !agentSelfUninstallRequestMatches(base, "node-guid") {
		t.Fatal("valid node-bound uninstall command was rejected")
	}
	for name, changed := range map[string]AgentRuntimeConfig{
		"wrong node":  func() AgentRuntimeConfig { value := base; value.NodeGUID = "other-node"; return value }(),
		"bad request": func() AgentRuntimeConfig { value := base; value.UninstallRequestID = "../delete"; return value }(),
		"bad schema":  func() AgentRuntimeConfig { value := base; value.SchemaVersion++; return value }(),
	} {
		t.Run(name, func(t *testing.T) {
			if agentSelfUninstallRequestMatches(changed, "node-guid") {
				t.Fatal("mismatched uninstall command was accepted")
			}
		})
	}
	withCommand := base
	withCommand.ConfigHash = ""
	withoutCommand := withCommand
	withoutCommand.UninstallRequestID = ""
	withHash, err := agentConfigHash(withCommand)
	if err != nil {
		t.Fatal(err)
	}
	withoutHash, err := agentConfigHash(withoutCommand)
	if err != nil {
		t.Fatal(err)
	}
	if withHash != withoutHash {
		t.Fatal("one-shot uninstall command changed the route configuration hash")
	}
}

func TestOldUninstallCompletionCannotRemoveDifferentNodeState(t *testing.T) {
	completion := agentUninstallCompletion{NodeGUID: "deleted-node", RequestID: "0123456789abcdef0123456789abcdef"}
	if !agentUninstallRecoveryMayRemoveState(completion, edgeAgentState{NodeGUID: "deleted-node", Token: "old-token"}) {
		t.Fatal("completion for the same enrolled node should remove its Agent state")
	}
	for name, state := range map[string]edgeAgentState{
		"new node":      {NodeGUID: "new-node", Token: "new-token"},
		"unenrolled":    {NodeGUID: "", Token: ""},
		"missing token": {NodeGUID: "deleted-node", Token: ""},
	} {
		t.Run(name, func(t *testing.T) {
			if agentUninstallRecoveryMayRemoveState(completion, state) {
				t.Fatal("old completion was allowed to remove state it does not own")
			}
		})
	}
	if validAgentUninstallCompletion(agentUninstallCompletion{NodeGUID: "deleted-node", RequestID: "../bad"}) {
		t.Fatal("invalid completion request ID was accepted")
	}
}

func TestAgentUninstallRecoveryPathsAreRequestScoped(t *testing.T) {
	first, err := agentUninstallRecoveryPathsFor("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	second, err := agentUninstallRecoveryPathsFor("abcdef0123456789abcdef0123456789")
	if err != nil {
		t.Fatal(err)
	}
	if first.ServiceName == second.ServiceName || first.Binary == second.Binary || first.Unit == second.Unit || first.Init == second.Init || first.Done == second.Done || first.Lock == second.Lock {
		t.Fatal("separate uninstall requests share a privileged recovery path")
	}
	if _, err := agentUninstallRecoveryPathsFor("../untrusted"); err == nil {
		t.Fatal("invalid request ID was used to construct recovery paths")
	}
}

func TestDeleteNodeAPIReportsPendingUntilAuthenticatedAgentCompletion(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "delete API", Address: "203.0.113.50"}, now)
	if err != nil {
		t.Fatal(err)
	}
	enrolled, token, err := app.db.EnrollControlNode(enrollment, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	deleteRequest := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/nodes/%d", node.ID), nil)
	deleteResponse := httptest.NewRecorder()
	app.handleNodeByID(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleteResponse.Code, deleteResponse.Body.String())
	}
	var deleteResult map[string]any
	if err := json.Unmarshal(deleteResponse.Body.Bytes(), &deleteResult); err != nil {
		t.Fatal(err)
	}
	if deleteResult["deleted"] != false || deleteResult["uninstall_pending"] != true || deleteResult["agent_online"] != false {
		t.Fatalf("unexpected offline delete result: %s", deleteResponse.Body.String())
	}
	pending, err := app.db.controlNodeByID(node.ID, now)
	if err != nil || !pending.UninstallPending || pending.Enabled {
		t.Fatalf("node record must remain disabled/pending until remote ACK: node=%+v err=%v", pending, err)
	}

	body, _ := json.Marshal(map[string]string{"node_guid": enrolled.GUID, "request_id": pending.uninstallRequestID})
	ackRequest := httptest.NewRequest(http.MethodPost, "/api/agent/uninstall/complete", bytes.NewReader(body))
	ackRequest.Header.Set("Authorization", "Bearer "+token)
	ackResponse := httptest.NewRecorder()
	app.handleAgentUninstallComplete(ackResponse, ackRequest)
	if ackResponse.Code != http.StatusOK {
		t.Fatalf("Agent completion ACK status=%d body=%s", ackResponse.Code, ackResponse.Body.String())
	}
	if _, err := app.db.controlNodeByID(node.ID, now); !errors.Is(err, errNodeNotFound) {
		t.Fatalf("node row remains after authenticated remote completion: %v", err)
	}
}

func TestNodeUninstallCannotBeQueuedUntilScheduleCleanupIsComplete(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "cleanup gates uninstall", Address: "203.0.113.52"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.db.EnrollControlNode(enrollment, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "cleanup gate", PublicHost: "gate.example.test", TargetURL: "http://127.0.0.1:8096"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.MarkNodeDeletionPending(node.ID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.db.BeginNodeUninstall(node.ID, now.Add(3*time.Second)); err == nil {
		t.Fatal("Agent uninstall was queued while a site schedule still referenced the node")
	}
	pending, err := app.db.controlNodeByID(node.ID, now)
	if err != nil || !pending.DeletionPending || pending.UninstallPending || pending.Enabled {
		t.Fatalf("failed cleanup must leave a disabled, retryable deletion phase without sending Agent command: node=%+v err=%v", pending, err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now.Add(4*time.Second)); err == nil {
		t.Fatal("a pending-deletion node was accepted as a new fixed assignment")
	}
	if _, err := app.db.db.Exec(`UPDATE site_node_schedules SET enabled=0,fixed_node_id=NULL,desired_node_id=NULL,applied_node_id=NULL WHERE site_id=?`, site.ID); err != nil {
		t.Fatal(err)
	}
	queued, hasAgent, err := app.db.BeginNodeUninstall(node.ID, now.Add(5*time.Second))
	if err != nil || !hasAgent || !queued.UninstallPending {
		t.Fatalf("uninstall was not queued after schedule cleanup: node=%+v hasAgent=%v err=%v", queued, hasAgent, err)
	}
}

func TestPrepareNodeDeletionRereadsScheduleAfterAcquiringSiteLock(t *testing.T) {
	f := newSchedulerDNSFixture(t, false)
	other, _, err := f.app.db.CreateControlNode(NodeCreateInput{Name: "replacement", Address: "203.0.113.6", Port: 443}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.db.MarkNodeDeletionPending(f.node.ID, f.now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	unlock := f.app.lockSiteSchedule(f.site.ID)
	finished := make(chan error, 1)
	go func() { finished <- f.app.prepareNodeDeletion(context.Background(), f.node.ID) }()
	deadline := time.Now().Add(time.Second)
	for {
		f.app.siteScheduleLocksMu.Lock()
		lock := f.app.siteScheduleLocks[f.site.ID]
		waiting := lock != nil && lock.refs >= 2
		f.app.siteScheduleLocksMu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			unlock()
			t.Fatal("node deletion did not wait for the held site lock")
		}
		time.Sleep(time.Millisecond)
	}
	marker := siteDNSOwnershipMarker(f.site.ID, f.app.db.installUUID)
	if _, err := f.app.db.db.Exec(`UPDATE site_node_schedules SET fixed_node_id=?,desired_node_id=?,applied_node_id=?,cf_record_id='record-b',applied_address='203.0.113.6',schedule_revision=schedule_revision+1 WHERE site_id=?`, other.ID, other.ID, other.ID, f.site.ID); err != nil {
		unlock()
		t.Fatal(err)
	}
	f.mu.Lock()
	delete(f.records, "record-a")
	f.records["record-b"] = cloudflareAddressRecord{ID: "record-b", Type: "A", Name: f.host, Content: "203.0.113.6", Comment: marker}
	f.mu.Unlock()
	unlock()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("cleanup of a schedule already moved to another node failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("node deletion did not finish after site lock was released")
	}
	current, err := f.app.db.siteNodeSchedule(f.site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !current.Enabled || current.FixedNodeID != other.ID || current.DesiredNodeID != other.ID || current.AppliedNodeID != other.ID || current.cfRecordID != "record-b" {
		t.Fatalf("deleting old node damaged the replacement assignment: %+v", current)
	}
	if f.deletes.Load() != 0 {
		t.Fatalf("deleting old node removed replacement Cloudflare record: deletes=%d", f.deletes.Load())
	}
}

func TestSchema63AddsPendingDeletionStateToSchema62(t *testing.T) {
	app := newTestApp(t)
	node, _, err := app.db.CreateControlNode(NodeCreateInput{Name: "schema 63", Address: "203.0.113.51"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`DROP INDEX IF EXISTS idx_control_nodes_uninstall`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`ALTER TABLE control_nodes DROP COLUMN deletion_pending`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`PRAGMA user_version=62`); err != nil {
		t.Fatal(err)
	}
	if err := app.db.migrate(); err != nil {
		t.Fatal(err)
	}
	var schema int
	if err := app.db.db.QueryRow(`PRAGMA user_version`).Scan(&schema); err != nil || schema != 63 {
		t.Fatalf("schema=%d err=%v, want 63", schema, err)
	}
	upgraded, err := app.db.controlNodeByID(node.ID, time.Now().UTC())
	if err != nil || upgraded.UninstallPending || upgraded.UninstallRequestedAtMS != 0 || upgraded.DeletionPending {
		t.Fatalf("schema migration should leave existing nodes unaffected: node=%+v err=%v", upgraded, err)
	}
}

func TestForceDeletePendingNodeAndUnenrolledNodeDoNotWaitForAgent(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	unenrolled, _, err := app.db.CreateControlNode(NodeCreateInput{Name: "not installed", Address: "203.0.113.48"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, queued, err := app.db.BeginNodeUninstall(unenrolled.ID, now.Add(time.Second)); err != nil || queued {
		t.Fatalf("unenrolled node should not queue a remote operation: queued=%v err=%v", queued, err)
	}
	if err := app.db.DeleteControlNode(unenrolled.ID); err != nil {
		t.Fatal(err)
	}

	created, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "offline agent", Address: "203.0.113.49"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.db.EnrollControlNode(enrollment, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if pending, queued, err := app.db.BeginNodeUninstall(created.ID, now.Add(2*time.Second)); err != nil || !queued || !pending.UninstallPending {
		t.Fatalf("offline enrolled node should retain a pending uninstall: pending=%+v queued=%v err=%v", pending, queued, err)
	}
	if err := app.db.ForceDeletePendingNode(created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.controlNodeByID(created.ID, now); !errors.Is(err, errNodeNotFound) {
		t.Fatalf("forced removal did not delete pending node row: %v", err)
	}
}
