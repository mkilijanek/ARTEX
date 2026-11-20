package db

import (
	"errors"
	"testing"
	"time"
)

// Tests of the archive refusal (docs/detector-plan.md section 6.10, criterion 1a-16): a task that has a detection
// run is not archived, because task archives do not carry the detection tables yet. The refusal is held at three
// places: when the job is queued, inside the purge transaction, and when a run is started.

const detArchiveMessage = "this task has detection results, which task archives do not carry yet; delete them from the findings or upgrade to a release that archives them"

// archiveTask is a paused task with one finding, ready to be archived.
func (x *detLayer) archiveTask() (taskID, findingID int64) {
	x.t.Helper()
	task, err := x.d.CreateTask("detection archive task", "goal", nil, 0, 0)
	if err != nil {
		x.t.Fatal(err)
	}
	if err := x.d.SetPaused(task.ID, true); err != nil {
		x.t.Fatal(err)
	}
	f := x.finding()
	x.exec(`UPDATE findings SET task_id=$1 WHERE id=$2`, task.ID, f)
	return task.ID, f
}

// archiveUntilPurge queues the job, claims it and takes the snapshot: the point at which the server calls
// CompleteTaskArchive.
func (x *detLayer) archiveUntilPurge(taskID int64) (*TaskArchive, *TaskArchiveSnapshot) {
	x.t.Helper()
	job, err := x.d.QueueTaskArchive(taskID)
	if err != nil {
		x.t.Fatalf("queue the archive: %v", err)
	}
	if claimed, err := x.d.ClaimTaskArchiveJob(x.ctx); err != nil || claimed == nil || claimed.ID != job.ID {
		x.t.Fatalf("claim the archive job: %+v %v", claimed, err)
	}
	snap, err := x.d.SnapshotTaskArchive(taskID)
	if err != nil {
		x.t.Fatalf("snapshot the task: %v", err)
	}
	return job, snap
}

func (x *detLayer) archiveState(id int64) (state, message string) {
	x.t.Helper()
	return x.text(`SELECT state FROM task_archives WHERE id=$1`, id), x.text(`SELECT error FROM task_archives WHERE id=$1`, id)
}

// Queueing for a task with a run is refused with the message of 6.10 and changes nothing; once the run is deleted
// the job is queued; a task without runs, and a task whose only run is a tombstone, are queued.
func TestFindingDetectionArchiveQueueingIsRefused(t *testing.T) {
	x := newDetLayer(t, "detarchq")
	taskID, f := x.archiveTask()
	run := x.runningRun(f)
	if err := x.d.FinishFindingDetection(x.ctx, run, DetectionFinish{Status: "completed", Outcome: "unvalidated"}); err != nil {
		t.Fatal(err)
	}

	_, err := x.d.QueueTaskArchive(taskID)
	if !errors.Is(err, ErrTaskArchiveDetections) {
		t.Fatalf("queue with a run: err = %v, want ErrTaskArchiveDetections", err)
	}
	if err == nil || err.Error() != detArchiveMessage {
		t.Errorf("message = %q, want %q", err, detArchiveMessage)
	}
	if n := x.count(`SELECT count(*) FROM task_archives WHERE task_id=$1`, taskID); n != 0 {
		t.Errorf("a refused queueing left %d job rows", n)
	}
	if n := x.count(`SELECT count(*) FROM findings WHERE id=$1`, f); n != 1 {
		t.Error("a refused queueing changed the finding")
	}
	if n := x.count(`SELECT count(*) FROM finding_detections WHERE finding_id=$1`, f); n != 1 {
		t.Errorf("a refused queueing changed the runs: %d rows", n)
	}

	// The operator deletes the run: the same call now queues. The run was finished, so it is deleted for good.
	if tomb, err := x.d.DeleteFindingDetection(x.ctx, run); err != nil || tomb {
		t.Fatalf("delete the manual run: tombstone=%t err=%v", tomb, err)
	}
	if _, err := x.d.QueueTaskArchive(taskID); err != nil {
		t.Errorf("queue after the run was deleted: %v", err)
	}

	// A task whose only run is a tombstone is queued: tombstones carry no results.
	task2, f2 := x.archiveTask()
	auto, _, _, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f2, Path: "engine", TriggerKind: "auto", AutoAttempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	x.exec(`UPDATE finding_detections SET status='failed', finished_at=now() WHERE id=$1`, auto.ID)
	if tomb, err := x.d.DeleteFindingDetection(x.ctx, auto.ID); err != nil || !tomb {
		t.Fatalf("tombstone: %t %v", tomb, err)
	}
	if _, err := x.d.QueueTaskArchive(task2); err != nil {
		t.Errorf("queue with only a tombstone: %v", err)
	}
}

// A task without any run archives as before: the purge deletes its findings.
func TestFindingDetectionArchiveWithoutRunsStillPurges(t *testing.T) {
	x := newDetLayer(t, "detarchok")
	taskID, f := x.archiveTask()
	job, snap := x.archiveUntilPurge(taskID)
	if err := x.d.CompleteTaskArchive(job.ID, snap, "/nonexistent/detection-archive.tar.zst", "0", 1, 1); err != nil {
		t.Fatalf("purge of a task without runs: %v", err)
	}
	if n := x.count(`SELECT count(*) FROM findings WHERE id=$1`, f); n != 0 {
		t.Error("the purge left the finding")
	}
	if state, _ := x.archiveState(job.ID); state != ArchiveReady {
		t.Errorf("state = %q, want %q", state, ArchiveReady)
	}
}

// A run created after the job was queued and before the purge (the start check cannot see a job that did not yet
// exist) makes the purge fail with the same message: nothing is deleted and the job is a failed archive.
func TestFindingDetectionArchivePurgeRefusesARunMadeAfterQueueing(t *testing.T) {
	x := newDetLayer(t, "detarchp")
	taskID, f := x.archiveTask()
	job, snap := x.archiveUntilPurge(taskID)

	// The run that wins the race with the queueing: made while no job is active (the job is put back after).
	x.exec(`UPDATE task_archives SET state='archive_failed' WHERE id=$1`, job.ID)
	run := x.create(f, "engine")
	if _, err := x.d.RecordDetectionRule(x.ctx, x.mustRunning(run.ID), sigmaRule("kept")); err != nil {
		t.Fatal(err)
	}
	x.exec(`UPDATE task_archives SET state='archiving' WHERE id=$1`, job.ID)

	err := x.d.CompleteTaskArchive(job.ID, snap, "/nonexistent/detection-archive.tar.zst", "0", 1, 1)
	if !errors.Is(err, ErrTaskArchiveDetections) {
		t.Fatalf("purge with a run: err = %v, want ErrTaskArchiveDetections", err)
	}
	if err == nil || err.Error() != detArchiveMessage {
		t.Errorf("message = %q, want %q", err, detArchiveMessage)
	}
	if n := x.count(`SELECT count(*) FROM findings WHERE id=$1`, f); n != 1 {
		t.Error("a refused purge deleted the finding")
	}
	if n := x.count(`SELECT count(*) FROM finding_detections WHERE id=$1`, run.ID); n != 1 {
		t.Error("a refused purge deleted the run")
	}
	if n := x.count(`SELECT count(*) FROM detection_rules WHERE detection_id=$1`, run.ID); n != 1 {
		t.Error("a refused purge deleted the rule")
	}
	if n := x.count(`SELECT count(*) FROM tasks WHERE id=$1 AND deleted_at IS NULL AND archived_at IS NULL`, taskID); n != 1 {
		t.Error("a refused purge changed the task")
	}

	// What the server does with the error: the job ends as a failed archive that shows the message.
	if err := x.d.FailTaskArchiveJob(job.ID, Archiving, err); err != nil {
		t.Fatal(err)
	}
	if state, msg := x.archiveState(job.ID); state != ArchiveFailed || msg != detArchiveMessage {
		t.Errorf("job = %q / %q, want %q / the message", state, msg, ArchiveFailed)
	}
}

// mustRunning makes the run running (as a claim does) and returns its id, for the rule that is recorded next.
func (x *detLayer) mustRunning(id int64) int64 {
	x.t.Helper()
	x.running(id)
	return id
}

// With a purge holding the findings, a concurrent create waits for it and then finds no finding.
func TestFindingDetectionCreateWaitsForThePurgeLock(t *testing.T) {
	x := newDetLayer(t, "detarchl")
	taskID, f := x.archiveTask()

	tx, err := x.d.BeginTx(x.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(x.ctx, `SELECT id FROM findings WHERE task_id=$1 FOR UPDATE`, taskID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, _, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f, Path: "engine"})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the create did not wait for the lock on the findings: %v", err)
	case <-time.After(400 * time.Millisecond):
	}
	if _, err := tx.ExecContext(x.ctx, `DELETE FROM findings WHERE task_id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrFindingNotFound) {
			t.Errorf("create after the purge: err = %v, want ErrFindingNotFound", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the create did not return after the purge committed")
	}
	if n := x.count(`SELECT count(*) FROM finding_detections`); n != 0 {
		t.Errorf("%d runs exist for a purged finding", n)
	}
}

// A create for a finding whose task has a queued or running archive job is refused, and changes nothing; a failed
// archive job does not block it.
func TestFindingDetectionCreateIsRefusedWhileTheTaskIsArchived(t *testing.T) {
	x := newDetLayer(t, "detarchs")
	taskID, f := x.archiveTask()
	job, err := x.d.QueueTaskArchive(taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"archive_queued", "archiving"} {
		x.exec(`UPDATE task_archives SET state=$2 WHERE id=$1`, job.ID, state)
		for _, path := range []string{"engine", "agent"} {
			_, _, created, err := x.d.CreateFindingDetection(x.ctx, CreateDetectionInput{FindingID: f, Path: path})
			if !errors.Is(err, ErrDetectionArchiveActive) || created {
				t.Errorf("%s create while %s: created=%t err=%v, want ErrDetectionArchiveActive", path, state, created, err)
			}
		}
	}
	if n := x.count(`SELECT count(*) FROM finding_detections`); n != 0 {
		t.Errorf("refused creates left %d runs", n)
	}
	if n := x.count(`SELECT count(*) FROM conversations WHERE agent_key=$1`, FindingDetectionAgentKey); n != 0 {
		t.Errorf("a refused agent create left %d conversations", n)
	}
	x.exec(`UPDATE task_archives SET state='archive_failed' WHERE id=$1`, job.ID)
	x.create(f, "engine")

	// A finding of a task that was never archived is unaffected.
	_, other := x.archiveTask()
	x.create(other, "engine")
}
