package domain

import (
	"strings"
	"testing"
)

func validTask() Task {
	return Task{
		OwnerUserID:    "usr_01",
		Source:         TaskSourceGoalEngine,
		BotID:          "bot_sales",
		ChannelID:      "chan_ops",
		Brief:          "Revenue is 12% behind pace. Draft a plan and report back.",
		IdempotencyKey: "goal_7:2026-09-11",
		Status:         TaskStatusQueued,
	}
}

func TestTask_validTask_passes(t *testing.T) {
	// Arrange
	task := validTask()

	// Act
	err := task.Validate()

	// Assert
	if err != nil {
		t.Fatalf("a well-formed task was rejected: %v", err)
	}
}

func TestTask_missingOwner_isRejected(t *testing.T) {
	// Arrange
	task := validTask()
	task.OwnerUserID = "  "

	// Act
	fields := fieldsOf(t, task.Validate())

	// Assert
	// A run whose tokens belong to nobody cannot be capped, so ownerless work must
	// not reach the loop.
	if _, found := fields["ownerUserId"]; !found {
		t.Errorf("an ownerless task was accepted; got %v", fields)
	}
}

func TestTask_emptyBrief_isRejected(t *testing.T) {
	// Arrange
	task := validTask()
	task.Brief = "\n\t "

	// Act
	fields := fieldsOf(t, task.Validate())

	// Assert
	// Whitespace is not an instruction. Accepting it would spend a model call to
	// discover there was nothing to do.
	if _, found := fields["brief"]; !found {
		t.Errorf("a whitespace-only brief was accepted; got %v", fields)
	}
}

func TestTask_oversizedBrief_isRejected(t *testing.T) {
	// Arrange
	task := validTask()
	task.Brief = strings.Repeat("a", MaxBriefLength+1)

	// Act
	fields := fieldsOf(t, task.Validate())

	// Assert
	if _, found := fields["brief"]; !found {
		t.Errorf("an oversized brief was accepted; got %v", fields)
	}
}

// The bound counts characters, not bytes, so a brief written in a language that
// needs multi-byte runes is not held to a shorter limit than an English one.
func TestTask_briefAtTheLimitInMultibyteCharacters_isAccepted(t *testing.T) {
	// Arrange
	task := validTask()
	task.Brief = strings.Repeat("あ", MaxBriefLength)

	// Act
	err := task.Validate()

	// Assert
	if err != nil {
		t.Fatalf("a brief of exactly %d characters was rejected: %v", MaxBriefLength, err)
	}
}

func TestTask_unknownSource_isRejected(t *testing.T) {
	// Arrange
	task := validTask()
	task.Source = TaskSource("cron")

	// Act
	fields := fieldsOf(t, task.Validate())

	// Assert
	// An unrecognised source would be treated as unattended by Attended(), which is
	// safe — but silently reclassifying somebody's work is not, so reject it here.
	if _, found := fields["source"]; !found {
		t.Errorf("an unknown source was accepted; got %v", fields)
	}
}

func TestTask_unknownStatus_isRejected(t *testing.T) {
	// Arrange
	task := validTask()
	task.Status = TaskStatus("done")

	// Act
	fields := fieldsOf(t, task.Validate())

	// Assert
	if _, found := fields["status"]; !found {
		t.Errorf("an unknown status was accepted; got %v", fields)
	}
}

func TestTask_oversizedMetadata_isRejected(t *testing.T) {
	// Arrange
	tooMany := validTask()
	tooMany.Metadata = make(map[string]string, MaxMetadataEntries+1)
	for i := 0; i <= MaxMetadataEntries; i++ {
		tooMany.Metadata[string(rune('a'+i))+"key"] = "v"
	}

	tooLong := validTask()
	tooLong.Metadata = map[string]string{"note": strings.Repeat("x", MaxMetadataValueLength+1)}

	// Act
	tooManyFields := fieldsOf(t, tooMany.Validate())
	tooLongFields := fieldsOf(t, tooLong.Validate())

	// Assert
	if _, found := tooManyFields["metadata"]; !found {
		t.Errorf("too many metadata entries were accepted; got %v", tooManyFields)
	}
	if _, found := tooLongFields["metadata"]; !found {
		t.Errorf("an oversized metadata value was accepted; got %v", tooLongFields)
	}
}

func TestTask_reportsEveryProblemAtOnce(t *testing.T) {
	// Arrange
	task := Task{}

	// Act
	fields := fieldsOf(t, task.Validate())

	// Assert
	for _, field := range []string{"ownerUserId", "brief", "source", "status"} {
		if _, found := fields[field]; !found {
			t.Errorf("an empty task did not report %s; got %v", field, fields)
		}
	}
}

func TestTask_withDefaults_trimsTextAndQueuesTheTask(t *testing.T) {
	// Arrange
	task := Task{
		OwnerUserID:    " usr_01 ",
		BotID:          " bot_sales\n",
		ChannelID:      "\tchan_ops ",
		Brief:          "  do the thing  ",
		IdempotencyKey: " key-1 ",
	}

	// Act
	got := task.WithDefaults()

	// Assert
	if got.OwnerUserID != "usr_01" || got.BotID != "bot_sales" || got.ChannelID != "chan_ops" {
		t.Errorf("identifiers were not trimmed: %+v", got)
	}
	if got.Brief != "do the thing" {
		t.Errorf("brief = %q; want it trimmed", got.Brief)
	}
	// An untrimmed key would make " key-1 " and "key-1" two different tasks, which
	// defeats the point of the header.
	if got.IdempotencyKey != "key-1" {
		t.Errorf("idempotencyKey = %q; want it trimmed", got.IdempotencyKey)
	}
	if got.Status != TaskStatusQueued {
		t.Errorf("status = %q; want %q", got.Status, TaskStatusQueued)
	}
}

func TestTask_withDefaults_leavesAnExplicitStatusAlone(t *testing.T) {
	// Arrange
	task := validTask()
	task.Status = TaskStatusRunning

	// Act
	got := task.WithDefaults()

	// Assert
	// Re-queueing a running task is how the same work gets done twice.
	if got.Status != TaskStatusRunning {
		t.Errorf("status = %q; a running task was requeued", got.Status)
	}
}

func TestTask_withDefaults_doesNotMutateTheReceiver(t *testing.T) {
	// Arrange
	task := Task{Brief: " padded "}

	// Act
	_ = task.WithDefaults()

	// Assert
	if task.Brief != " padded " {
		t.Errorf("the receiver was mutated: brief = %q", task.Brief)
	}
}

func TestTaskStatusFor_onlyCompletedSucceeds(t *testing.T) {
	// Arrange
	all := AllStopReasons()

	// Act & Assert
	for _, reason := range all {
		got := TaskStatusFor(reason)
		want := TaskStatusFailed
		if reason == StopCompleted {
			want = TaskStatusSucceeded
		}
		if got != want {
			t.Errorf("TaskStatusFor(%q) = %q; want %q", reason, got, want)
		}
	}
}

func TestRun_inFlightUntilAStopReasonIsRecorded(t *testing.T) {
	// Arrange
	running := Run{ID: "run_01"}
	finished := Run{ID: "run_01", Stop: StopCompleted}

	// Act & Assert
	if !running.InFlight() {
		t.Error("a run with no stop reason was reported as finished")
	}
	if finished.InFlight() {
		t.Error("a completed run was reported as in flight")
	}
}

func TestStep_tokens_sumsBothDirections(t *testing.T) {
	// Arrange
	step := Step{Kind: StepKindModel, TokensIn: 1_200, TokensOut: 340}

	// Act
	got := step.Tokens()

	// Assert
	// Billing counts both, so a budget that counted only output would undercharge
	// every long prompt — the exact shape of run this service produces.
	if got != 1_540 {
		t.Errorf("Tokens() = %d; want 1540", got)
	}
}

func TestTruncateContent_shortContentIsUntouched(t *testing.T) {
	// Arrange
	content := "all done"

	// Act
	got := TruncateContent(content)

	// Assert
	if got != content {
		t.Errorf("TruncateContent(%q) = %q; want it unchanged", content, got)
	}
}

func TestTruncateContent_longContentIsMarked(t *testing.T) {
	// Arrange
	content := strings.Repeat("x", MaxStepContentLength+500)

	// Act
	got := TruncateContent(content)

	// Assert
	// A reader has to be able to tell the transcript is not the whole output.
	if !strings.HasSuffix(got, "… truncated") {
		t.Error("truncated content was not marked as truncated")
	}
	if kept := len([]rune(strings.TrimSuffix(got, "\n… truncated"))); kept != MaxStepContentLength {
		t.Errorf("kept %d characters; want %d", kept, MaxStepContentLength)
	}
}

func TestTruncateContent_cutsOnRuneBoundaries(t *testing.T) {
	// Arrange
	content := strings.Repeat("あ", MaxStepContentLength+10)

	// Act
	got := TruncateContent(content)

	// Assert
	// Cutting a UTF-8 sequence in half produces a column of replacement characters
	// in every dashboard downstream.
	if strings.ContainsRune(got, '�') {
		t.Error("truncation split a multi-byte character")
	}
}
