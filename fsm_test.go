package swf

import (
	"log"
	"math/rand"
	"reflect"
	"strconv"
	"testing"
	"time"

	"os"

	"code.google.com/p/goprotobuf/proto"
)

//Todo add tests of error handling mechanism
//assert that the decisions have the mark and the signal external...hmm need workflow id for signal external.

var testActivityInfo = ActivityInfo{ActivityID: "activityId", ActivityType: ActivityType{Name: "activity", Version: "activityVersion"}}

func TestFSM(t *testing.T) {

	fsm := FSM{
		Name:       "test-fsm",
		DataType:   TestData{},
		Serializer: JSONStateSerializer{},
	}

	fsm.AddInitialState(&FSMState{
		Name: "start",
		Decider: func(f *FSMContext, lastEvent HistoryEvent, data interface{}) Outcome {
			testData := data.(*TestData)
			testData.States = append(testData.States, "start")
			serialized := f.Serialize(testData)
			decision := Decision{
				DecisionType: DecisionTypeScheduleActivityTask,
				ScheduleActivityTaskDecisionAttributes: &ScheduleActivityTaskDecisionAttributes{
					ActivityID:   testActivityInfo.ActivityID,
					ActivityType: testActivityInfo.ActivityType,
					TaskList:     &TaskList{Name: "taskList"},
					Input:        serialized,
				},
			}

			return f.Goto("working", testData, []Decision{decision})

		},
	})

	fsm.AddState(&FSMState{
		Name: "working",
		Decider: TypedDecider(func(f *FSMContext, lastEvent HistoryEvent, testData *TestData) Outcome {
			testData.States = append(testData.States, "working")
			serialized := f.Serialize(testData)
			var decisions = f.EmptyDecisions()
			if lastEvent.EventType == EventTypeActivityTaskCompleted {
				decision := Decision{
					DecisionType: DecisionTypeCompleteWorkflowExecution,
					CompleteWorkflowExecutionDecisionAttributes: &CompleteWorkflowExecutionDecisionAttributes{
						Result: serialized,
					},
				}
				decisions = append(decisions, decision)
			} else if lastEvent.EventType == EventTypeActivityTaskFailed {
				decision := Decision{
					DecisionType: DecisionTypeScheduleActivityTask,
					ScheduleActivityTaskDecisionAttributes: &ScheduleActivityTaskDecisionAttributes{
						ActivityID:   testActivityInfo.ActivityID,
						ActivityType: testActivityInfo.ActivityType,
						TaskList:     &TaskList{Name: "taskList"},
						Input:        serialized,
					},
				}
				decisions = append(decisions, decision)
			}

			return f.Stay(testData, decisions)
		}),
	})

	events := []HistoryEvent{
		HistoryEvent{EventType: "DecisionTaskStarted", EventID: 3},
		HistoryEvent{EventType: "DecisionTaskScheduled", EventID: 2},
		HistoryEvent{
			EventID:   1,
			EventType: "WorkflowExecutionStarted",
			WorkflowExecutionStartedEventAttributes: &WorkflowExecutionStartedEventAttributes{
				Input: "{\"States\":[]}",
			},
		},
	}

	first := &PollForDecisionTaskResponse{
		Events:                 events,
		PreviousStartedEventID: 0,
	}

	decisions, _ := fsm.Tick(first)

	if !Find(decisions, stateMarkerPredicate) {
		t.Fatal("No Record State Marker")
	}

	if !Find(decisions, scheduleActivityPredicate) {
		t.Fatal("No ScheduleActivityTask")
	}

	secondEvents := DecisionsToEvents(decisions)
	secondEvents = append(secondEvents, events...)

	if state, _ := fsm.findSerializedState(secondEvents); state.ReplicationData.StateName != "working" {
		t.Fatal("current state is not 'working'", secondEvents)
	}

	second := &PollForDecisionTaskResponse{
		Events:                 secondEvents,
		PreviousStartedEventID: 3,
	}

	secondDecisions, _ := fsm.Tick(second)

	if !Find(secondDecisions, stateMarkerPredicate) {
		t.Fatal("No Record State Marker")
	}

	if !Find(secondDecisions, completeWorkflowPredicate) {
		t.Fatal("No CompleteWorkflow")
	}

}

func Find(decisions []Decision, predicate func(Decision) bool) bool {
	return FindDecision(decisions, predicate) != nil
}

func FindDecision(decisions []Decision, predicate func(Decision) bool) *Decision {
	for _, d := range decisions {
		if predicate(d) {
			return &d
		}
	}
	return nil
}

func stateMarkerPredicate(d Decision) bool {
	return d.DecisionType == "RecordMarker" && d.RecordMarkerDecisionAttributes.MarkerName == StateMarker
}

func scheduleActivityPredicate(d Decision) bool {
	return d.DecisionType == "ScheduleActivityTask"
}

func completeWorkflowPredicate(d Decision) bool {
	return d.DecisionType == "CompleteWorkflowExecution"
}

func startTimerPredicate(d Decision) bool {
	return d.DecisionType == "StartTimer"
}

func DecisionsToEvents(decisions []Decision) []HistoryEvent {
	var events []HistoryEvent
	for _, d := range decisions {
		if scheduleActivityPredicate(d) {
			event := HistoryEvent{
				EventType: "ActivityTaskCompleted",
				EventID:   7,
				ActivityTaskCompletedEventAttributes: &ActivityTaskCompletedEventAttributes{
					ScheduledEventID: 6,
				},
			}
			events = append(events, event)
			event = HistoryEvent{
				EventType: "ActivityTaskScheduled",
				EventID:   6,
				ActivityTaskScheduledEventAttributes: &ActivityTaskScheduledEventAttributes{
					ActivityID:   testActivityInfo.ActivityID,
					ActivityType: testActivityInfo.ActivityType,
				},
			}
			events = append(events, event)
		}
		if stateMarkerPredicate(d) {
			event := HistoryEvent{
				EventType: "MarkerRecorded",
				EventID:   5,
				MarkerRecordedEventAttributes: &MarkerRecordedEventAttributes{
					MarkerName: StateMarker,
					Details:    d.RecordMarkerDecisionAttributes.Details,
				},
			}
			events = append(events, event)

		}
	}
	return events
}

type TestData struct {
	States []string
}

func TestMarshalledDecider(t *testing.T) {
	typedDecider := func(f *FSMContext, h HistoryEvent, d TestData) Outcome {
		if d.States[0] != "marshalled" {
			t.Fail()
		}
		return f.Goto("ok", d, nil)
	}

	wrapped := TypedDecider(typedDecider)

	outcome := wrapped(&FSMContext{}, HistoryEvent{}, TestData{States: []string{"marshalled"}})

	if outcome.(TransitionOutcome).state != "ok" {
		t.Fatal("NextState was not 'ok'")
	}
}

func TestPanicRecovery(t *testing.T) {
	s := &FSMState{
		Name: "panic",
		Decider: func(f *FSMContext, e HistoryEvent, data interface{}) Outcome {
			panic("can you handle it?")
		},
	}
	f := &FSM{}
	f.AddInitialState(s)
	_, err := f.panicSafeDecide(s, new(FSMContext), HistoryEvent{}, nil)
	if err == nil {
		t.Fatal("fatallz")
	} else {
		log.Println(err)
	}
}

func TestErrorHandling(t *testing.T) {
	fsm := FSM{
		Name:        "test-fsm",
		DataType:    TestData{},
		Serializer:  JSONStateSerializer{},
		allowPanics: true,
	}

	fsm.AddInitialState(&FSMState{
		Name: "ok",
		Decider: func(f *FSMContext, h HistoryEvent, d interface{}) Outcome {
			if h.EventType == EventTypeWorkflowExecutionStarted {
				return f.Stay(d, nil)
			}
			if h.EventType == EventTypeWorkflowExecutionSignaled && d.(*TestData).States[0] == "recovered" {
				log.Println("recovered")
				return f.Stay(d, nil)
			}
			t.Fatalf("ok state did not get recovered %s", h)
			return nil

		},
	})

	fsm.AddErrorState(&FSMState{
		Name: "error",
		Decider: func(f *FSMContext, h HistoryEvent, d interface{}) Outcome {
			if h.EventType == EventTypeWorkflowExecutionSignaled && h.WorkflowExecutionSignaledEventAttributes.SignalName == ErrorSignal {
				log.Println("in error recovery")
				return f.Goto("ok", &TestData{States: []string{"recovered"}}, nil)
			}
			t.Fatalf("error handler got unexpected event")
			return nil

		},
	})

	events := []HistoryEvent{
		HistoryEvent{
			EventID:   3,
			EventType: EventTypeWorkflowExecutionSignaled,
			WorkflowExecutionSignaledEventAttributes: &WorkflowExecutionSignaledEventAttributes{
				SignalName: "NOT AN ERROR",
				Input:      "Hi",
			},
		},
		HistoryEvent{
			EventID:   2,
			EventType: EventTypeWorkflowExecutionSignaled,
			WorkflowExecutionSignaledEventAttributes: &WorkflowExecutionSignaledEventAttributes{
				SignalName: ErrorSignal,
				Input:      "{\"workflowEpoch\":2}",
			},
		},
		HistoryEvent{
			EventID:   1,
			EventType: EventTypeWorkflowExecutionStarted,
			WorkflowExecutionStartedEventAttributes: &WorkflowExecutionStartedEventAttributes{
				Input: "",
			},
		},
	}

	resp := &PollForDecisionTaskResponse{
		Events:                 events,
		StartedEventID:         1,
		PreviousStartedEventID: 0,
	}

	decisions, _ := fsm.Tick(resp)
	if len(decisions) != 1 && decisions[0].DecisionType != DecisionTypeRecordMarker {
		t.Fatalf("no state marker!")
	}
	//Try with TypedDecider
	id := fsm.initialState.Decider
	fsm.initialState.Decider = TypedDecider(func(f *FSMContext, h HistoryEvent, d *TestData) Outcome { return id(f, h, d) })
	ie := fsm.errorState.Decider
	fsm.errorState.Decider = TypedDecider(func(f *FSMContext, h HistoryEvent, d *TestData) Outcome { return ie(f, h, d) })

	decisions2, _ := fsm.Tick(resp)
	if len(decisions2) != 1 && decisions2[0].DecisionType != DecisionTypeRecordMarker {
		t.Fatalf("no state marker for typed decider!")
	}
}

func TestProtobufSerialization(t *testing.T) {
	ser := &ProtobufStateSerializer{}
	key := "FOO"
	val := "BAR"
	init := &ConfigVar{Key: &key, Str: &val}
	serialized, err := ser.Serialize(init)
	if err != nil {
		t.Fatal(err)
	}

	log.Println(serialized)

	deserialized := new(ConfigVar)
	err = ser.Deserialize(serialized, deserialized)
	if err != nil {
		t.Fatal(err)
	}

	if init.GetKey() != deserialized.GetKey() {
		t.Fatal(init, deserialized)
	}

	if init.GetStr() != deserialized.GetStr() {
		t.Fatal(init, deserialized)
	}
}

//This is c&p from som generated code

type ConfigVar struct {
	Key             *string `protobuf:"bytes,1,req,name=key" json:"key,omitempty"`
	Str             *string `protobuf:"bytes,2,opt,name=str" json:"str,omitempty"`
	XXXunrecognized []byte  `json:"-"`
}

func (m *ConfigVar) Reset()         { *m = ConfigVar{} }
func (m *ConfigVar) String() string { return proto.CompactTextString(m) }
func (*ConfigVar) ProtoMessage()    {}

func (m *ConfigVar) GetKey() string {
	if m != nil && m.Key != nil {
		return *m.Key
	}
	return ""
}

func (m *ConfigVar) GetStr() string {
	if m != nil && m.Str != nil {
		return *m.Str
	}
	return ""
}

func ExampleFSM() {
	// create with swf.NewClient
	var client *Client
	// data type that will be managed by the FSM
	type StateData struct {
		Message string `json:"message,omitempty"`
		Count   int    `json:"count,omitempty"`
	}
	//event type that will be signalled to the FSM with signal name "hello"
	type Hello struct {
		Message string `json:"message,omitempty"`
	}
	//the FSM we will create will oscillate between 2 states,
	//waitForSignal -> will wait till the workflow is started or signalled, and update the StateData based on the Hello message received, set a timer, and transition to waitForTimer
	//waitForTimer -> will wait till the timer set by waitForSignal fires, and will signal the workflow with a Hello message, and transition to waitFotSignal
	waitForSignal := func(f *FSMContext, h HistoryEvent, d *StateData) Outcome {
		decisions := f.EmptyDecisions()
		switch h.EventType {
		case EventTypeWorkflowExecutionStarted, EventTypeWorkflowExecutionSignaled:
			if h.EventType == EventTypeWorkflowExecutionSignaled && h.WorkflowExecutionSignaledEventAttributes.SignalName == "hello" {
				hello := &Hello{}
				f.EventData(h, &Hello{})
				d.Count++
				d.Message = hello.Message
			}
			timeoutSeconds := "5" //swf uses stringy numbers in many places
			timerDecision := Decision{
				DecisionType: DecisionTypeStartTimer,
				StartTimerDecisionAttributes: &StartTimerDecisionAttributes{
					StartToFireTimeout: timeoutSeconds,
					TimerID:            "timeToSignal",
				},
			}
			decisions = append(decisions, timerDecision)
			return f.Goto("waitForTimer", d, decisions)
		}
		//if the event was unexpected just stay here
		return f.Stay(d, decisions)

	}

	waitForTimer := func(f *FSMContext, h HistoryEvent, d *StateData) Outcome {
		decisions := f.EmptyDecisions()
		switch h.EventType {
		case EventTypeTimerFired:
			//every time the timer fires, signal the workflow with a Hello
			message := strconv.FormatInt(time.Now().Unix(), 10)
			signalInput := &Hello{message}
			signalDecision := Decision{
				DecisionType: DecisionTypeSignalExternalWorkflowExecution,
				SignalExternalWorkflowExecutionDecisionAttributes: &SignalExternalWorkflowExecutionDecisionAttributes{
					SignalName: "hello",
					Input:      f.Serialize(signalInput),
					RunID:      f.RunID,
					WorkflowID: f.WorkflowID,
				},
			}
			decisions = append(decisions, signalDecision)

			return f.Goto("waitForSignal", d, decisions)
		}
		//if the event was unexpected just stay here
		return f.Stay(d, decisions)
	}

	//create the FSMState by passing the decider function through TypedDecider(),
	//which lets you use d *StateData rather than d interface{} in your decider.
	waitForSignalState := &FSMState{Name: "waitForSignal", Decider: TypedDecider(waitForSignal)}
	waitForTimerState := &FSMState{Name: "waitForTimer", Decider: TypedDecider(waitForTimer)}
	//wire it up in an fsm
	fsm := &FSM{
		Name:       "example-fsm",
		Client:     client,
		DataType:   StateData{},
		Domain:     "exaple-swf-domain",
		TaskList:   "example-decision-task-list-to-poll",
		Serializer: &JSONStateSerializer{},
	}
	//add states to FSM
	fsm.AddInitialState(waitForSignalState)
	fsm.AddState(waitForTimerState)

	//start it up!
	fsm.Start()
}

func TestChildRelator(t *testing.T) {
	start := StartWorkflowRequest{
		WorkflowID: "123",
		WorkflowType: WorkflowType{
			Name:    "test",
			Version: "123",
		},
	}

	relator := new(ChildRelator)

	relator.Relate("child.1", start.WorkflowID, start.WorkflowType)

	serialized, err := JSONStateSerializer{}.Serialize(relator)

	if err != nil {
		t.Fatal(err)
	}

	freshRelator := new(ChildRelator)

	JSONStateSerializer{}.Deserialize(serialized, freshRelator)

	if freshRelator.WorkflowID("child.1") != "123" {
		t.Fatal("id not 123")
	}

	c := freshRelator.WorkflowType("child.1")
	if c.Name != "test" {
		t.Fatal("name not test")
	}
	if c.Version != "123" {
		t.Fatal("version not test")
	}

	if os.Getenv("AWS_ACCESS_KEY_ID") == "" || os.Getenv("AWS_SECRET_ACCESS_KEY") == "" {
		log.Printf("WARNING: NO AWS CREDS SPECIFIED, SKIPPING CLIENTS TEST")
		return
	}

	client := NewClient(MustGetenv("AWS_ACCESS_KEY_ID"), MustGetenv("AWS_SECRET_ACCESS_KEY"), USEast1)
	client.Debug = true

	resp, err := freshRelator.WorkflowExecutionInfo(client, "swf4go", "123")
	if err != nil {
		t.Fatal(err)
	}

	if resp != nil {
		//nil is expected since there shouldnt be an execution with id 123
		t.Fatal(resp)
	}

}

func TestContinuedWorkflows(t *testing.T) {
	fsm := FSM{
		Name:       "test-fsm",
		DataType:   TestData{},
		Serializer: JSONStateSerializer{},
	}

	fsm.AddInitialState(&FSMState{
		Name: "ok",
		Decider: func(f *FSMContext, h HistoryEvent, d interface{}) Outcome {
			if h.EventType == EventTypeWorkflowExecutionSignaled && d.(*TestData).States[0] == "continuing" {
				return f.Stay(d, nil)
			}
			panic("broken")
		},
	})

	stateData := fsm.Serialize(TestData{States: []string{"continuing"}})
	state := SerializedState{
		ReplicationData: ReplicationData{
			WorkflowEpoch:  3,
			StartedEventID: 77,
			StateName:      "ok",
			StateData:      stateData,
		},
		PendingActivities: ActivityCorrelator{},
	}
	serializedState := fsm.Serialize(state)
	resp := &PollForDecisionTaskResponse{
		Events: []HistoryEvent{HistoryEvent{
			EventType: EventTypeWorkflowExecutionContinuedAsNew,
			WorkflowExecutionContinuedAsNewEventAttributes: &WorkflowExecutionContinuedAsNewEventAttributes{
				Input: serializedState,
			},
		}},
		StartedEventID: 5,
	}
	decisions, updatedState := fsm.Tick(resp)

	if updatedState.ReplicationData.StartedEventID != 5 {
		t.Fatal("startedEventId != 5")
	}

	if updatedState.ReplicationData.WorkflowEpoch != 4 {
		t.Fatal("workflowEpoch != 4")
	}

	if len(decisions) != 1 && decisions[0].RecordMarkerDecisionAttributes.MarkerName != StateMarker {
		t.Fatal("unexpected decisions")
	}
}

type MockKinesisClient struct {
	*Client
	putRecords []PutRecordRequest
	seqNumber  int
}

func (c *MockKinesisClient) PutRecord(req PutRecordRequest) (*PutRecordResponse, error) {
	if c.putRecords == nil {
		c.seqNumber = rand.Int()
		c.putRecords = make([]PutRecordRequest, 0)
	}
	c.putRecords = append(c.putRecords, req)
	c.seqNumber++
	return &PutRecordResponse{
		SequenceNumber: strconv.Itoa(c.seqNumber),
		ShardID:        req.PartitionKey,
	}, nil
}

func (c *MockKinesisClient) RespondDecisionTaskCompleted(request RespondDecisionTaskCompletedRequest) error {
	return nil
}

func TestKinesisReplication(t *testing.T) {
	client := &MockKinesisClient{}
	fsm := FSM{
		Client:        client,
		Name:          "test-fsm",
		DataType:      TestData{},
		KinesisStream: "test-stream",
		Serializer:    JSONStateSerializer{},
	}
	fsm.AddInitialState(&FSMState{
		Name: "initial",
		Decider: func(f *FSMContext, h HistoryEvent, d interface{}) Outcome {
			if h.EventType == EventTypeWorkflowExecutionStarted {
				return f.Goto("done", d, f.EmptyDecisions())
			}
			t.Fatal("unexpected")
			return nil // unreachable
		},
	})
	fsm.AddState(&FSMState{
		Name: "done",
		Decider: func(f *FSMContext, h HistoryEvent, d interface{}) Outcome {
			go fsm.PollerShutdownManager.StopPollers()
			return f.Stay(d, f.EmptyDecisions())
		},
	})
	events := []HistoryEvent{
		HistoryEvent{EventType: "DecisionTaskStarted", EventID: 3},
		HistoryEvent{EventType: "DecisionTaskScheduled", EventID: 2},
		HistoryEvent{
			EventID:   1,
			EventType: "WorkflowExecutionStarted",
			WorkflowExecutionStartedEventAttributes: &WorkflowExecutionStartedEventAttributes{
				Input: "{\"States\":[]}",
			},
		},
	}
	decisionTask := &PollForDecisionTaskResponse{
		Events:                 events,
		PreviousStartedEventID: 0,
	}
	fsm.handleDecisionTask(decisionTask)

	if client.putRecords == nil || len(client.putRecords) != 1 {
		t.Fatal("expected only one state to be replicated, got: %v", client.putRecords)
	}
	replication := client.putRecords[0]
	if replication.StreamName != fsm.KinesisStream {
		t.Fatalf("expected Kinesis stream: %q, got %q", fsm.KinesisStream, replication.StreamName)
	}
	var replicatedState SerializedState
	if err := fsm.Serializer.Deserialize(string(replication.Data), &replicatedState); err != nil {
		t.Fatal(err)
	}
	if replicatedState.ReplicationData.StartedEventID != 0 {
		t.Fatal("state.StartedEventID != 0, got: %d", replicatedState.ReplicationData.StartedEventID)
	}
	if replicatedState.ReplicationData.StateName != "done" {
		t.Fatalf("current state being replicated is not 'done', got %q", replicatedState.ReplicationData.StateName)
	}
}

func TestTrackPendingActivities(t *testing.T) {
	fsm := &FSM{
		Name:       "test-fsm",
		DataType:   TestData{},
		Serializer: JSONStateSerializer{},
	}

	fsm.AddInitialState(&FSMState{
		Name: "start",
		Decider: func(f *FSMContext, lastEvent HistoryEvent, data interface{}) Outcome {
			testData := data.(*TestData)
			testData.States = append(testData.States, "start")
			serialized := f.Serialize(testData)
			decision := Decision{
				DecisionType: DecisionTypeScheduleActivityTask,
				ScheduleActivityTaskDecisionAttributes: &ScheduleActivityTaskDecisionAttributes{
					ActivityID:   testActivityInfo.ActivityID,
					ActivityType: testActivityInfo.ActivityType,
					TaskList:     &TaskList{Name: "taskList"},
					Input:        serialized,
				},
			}
			return f.Goto("working", testData, []Decision{decision})
		},
	})

	// Deciders should be able to retrieve info about the pending activity
	fsm.AddState(&FSMState{
		Name: "working",
		Decider: TypedDecider(func(f *FSMContext, lastEvent HistoryEvent, testData *TestData) Outcome {
			testData.States = append(testData.States, "working")
			serialized := f.Serialize(testData)
			var decisions = f.EmptyDecisions()
			if lastEvent.EventType == EventTypeActivityTaskCompleted {
				trackedActivityInfo := f.ActivityInfo(lastEvent)
				if !reflect.DeepEqual(*trackedActivityInfo, testActivityInfo) {
					t.Fatalf("pending activity not being tracked\nExpected:\n%+v\nGot:\n%+v",
						testActivityInfo, trackedActivityInfo,
					)
				}
				timeoutSeconds := "5" //swf uses stringy numbers in many places
				decision := Decision{
					DecisionType: DecisionTypeStartTimer,
					StartTimerDecisionAttributes: &StartTimerDecisionAttributes{
						StartToFireTimeout: timeoutSeconds,
						TimerID:            "timeToComplete",
					},
				}
				return f.Goto("done", testData, []Decision{decision})
			} else if lastEvent.EventType == EventTypeActivityTaskFailed {
				trackedActivityInfo := f.ActivityInfo(lastEvent)
				if !reflect.DeepEqual(*trackedActivityInfo, testActivityInfo) {
					t.Fatalf("pending activity not being tracked\nExpected:\n%+v\nGot:\n%+v",
						testActivityInfo, trackedActivityInfo,
					)
				}
				decision := Decision{
					DecisionType: DecisionTypeScheduleActivityTask,
					ScheduleActivityTaskDecisionAttributes: &ScheduleActivityTaskDecisionAttributes{
						ActivityID:   testActivityInfo.ActivityID,
						ActivityType: testActivityInfo.ActivityType,
						TaskList:     &TaskList{Name: "taskList"},
						Input:        serialized,
					},
				}
				decisions = append(decisions, decision)
			}
			return f.Stay(testData, decisions)
		}),
	})

	// Pending activities are cleared after finished
	fsm.AddState(&FSMState{
		Name: "done",
		Decider: TypedDecider(func(f *FSMContext, lastEvent HistoryEvent, testData *TestData) Outcome {
			decisions := f.EmptyDecisions()
			if lastEvent.EventType == EventTypeTimerFired {
				testData.States = append(testData.States, "done")
				serialized := f.Serialize(testData)
				trackedActivityInfo := f.ActivityInfo(lastEvent)
				if trackedActivityInfo != nil {
					t.Fatalf("pending activity not being cleared\nGot:\n%+v", trackedActivityInfo)
				}
				decision := Decision{
					DecisionType: DecisionTypeCompleteWorkflowExecution,
					CompleteWorkflowExecutionDecisionAttributes: &CompleteWorkflowExecutionDecisionAttributes{
						Result: serialized,
					},
				}
				decisions = append(decisions, decision)
			}
			return f.Stay(testData, decisions)
		}),
	})

	// Schedule a task
	events := []HistoryEvent{
		HistoryEvent{EventType: "DecisionTaskStarted", EventID: 3},
		HistoryEvent{EventType: "DecisionTaskScheduled", EventID: 2},
		HistoryEvent{
			EventID:   1,
			EventType: "WorkflowExecutionStarted",
			WorkflowExecutionStartedEventAttributes: &WorkflowExecutionStartedEventAttributes{
				Input: "{\"States\":[]}",
			},
		},
	}
	first := &PollForDecisionTaskResponse{
		Events:                 events,
		PreviousStartedEventID: 0,
	}
	decisions, _ := fsm.Tick(first)
	recordMarker := FindDecision(decisions, stateMarkerPredicate)
	if recordMarker == nil {
		t.Fatal("No Record State Marker")
	}
	if !Find(decisions, scheduleActivityPredicate) {
		t.Fatal("No ScheduleActivityTask")
	}

	// Fail the task
	secondEvents := []HistoryEvent{
		{
			EventType: "ActivityTaskFailed",
			EventID:   7,
			ActivityTaskFailedEventAttributes: &ActivityTaskFailedEventAttributes{
				ScheduledEventID: 6,
			},
		},
		{
			EventType: "ActivityTaskScheduled",
			EventID:   6,
			ActivityTaskScheduledEventAttributes: &ActivityTaskScheduledEventAttributes{
				ActivityID:   testActivityInfo.ActivityID,
				ActivityType: testActivityInfo.ActivityType,
			},
		},
		{
			EventType: "MarkerRecorded",
			EventID:   5,
			MarkerRecordedEventAttributes: &MarkerRecordedEventAttributes{
				MarkerName: StateMarker,
				Details:    recordMarker.RecordMarkerDecisionAttributes.Details,
			},
		},
	}
	secondEvents = append(secondEvents, events...)
	if state, _ := fsm.findSerializedState(secondEvents); state.ReplicationData.StateName != "working" {
		t.Fatal("current state is not 'working'", secondEvents)
	}
	second := &PollForDecisionTaskResponse{
		Events:                 secondEvents,
		PreviousStartedEventID: 3,
	}
	secondDecisions, _ := fsm.Tick(second)
	recordMarker = FindDecision(secondDecisions, stateMarkerPredicate)
	if recordMarker == nil {
		t.Fatal("No Record State Marker")
	}
	if !Find(secondDecisions, scheduleActivityPredicate) {
		t.Fatal("No ScheduleActivityTask (retry)")
	}

	// Complete the task
	thirdEvents := []HistoryEvent{
		{
			EventType: "ActivityTaskCompleted",
			EventID:   11,
			ActivityTaskCompletedEventAttributes: &ActivityTaskCompletedEventAttributes{
				ScheduledEventID: 10,
			},
		},
		{
			EventType: "ActivityTaskScheduled",
			EventID:   10,
			ActivityTaskScheduledEventAttributes: &ActivityTaskScheduledEventAttributes{
				ActivityID:   testActivityInfo.ActivityID,
				ActivityType: testActivityInfo.ActivityType,
			},
		},
		{
			EventType: "MarkerRecorded",
			EventID:   9,
			MarkerRecordedEventAttributes: &MarkerRecordedEventAttributes{
				MarkerName: StateMarker,
				Details:    recordMarker.RecordMarkerDecisionAttributes.Details,
			},
		},
	}
	thirdEvents = append(thirdEvents, secondEvents...)
	if state, _ := fsm.findSerializedState(thirdEvents); state.ReplicationData.StateName != "working" {
		t.Fatal("current state is not 'working'", thirdEvents)
	}
	third := &PollForDecisionTaskResponse{
		Events:                 thirdEvents,
		PreviousStartedEventID: 7,
	}
	thirdDecisions, _ := fsm.Tick(third)
	recordMarker = FindDecision(thirdDecisions, stateMarkerPredicate)
	if recordMarker == nil {
		t.Fatal("No Record State Marker")
	}
	if !Find(thirdDecisions, startTimerPredicate) {
		t.Fatal("No StartTimer")
	}

	// Finish the workflow, check if pending activities were cleared
	fourthEvents := []HistoryEvent{
		{
			EventType: "TimerFired",
			EventID:   14,
		},
		{
			EventType: "MarkerRecorded",
			EventID:   13,
			MarkerRecordedEventAttributes: &MarkerRecordedEventAttributes{
				MarkerName: StateMarker,
				Details:    recordMarker.RecordMarkerDecisionAttributes.Details,
			},
		},
	}
	fourthEvents = append(fourthEvents, thirdEvents...)
	if state, _ := fsm.findSerializedState(fourthEvents); state.ReplicationData.StateName != "done" {
		t.Fatal("current state is not 'done'", fourthEvents)
	}
	fourth := &PollForDecisionTaskResponse{
		Events:                 fourthEvents,
		PreviousStartedEventID: 11,
	}
	fourthDecisions, _ := fsm.Tick(fourth)
	recordMarker = FindDecision(fourthDecisions, stateMarkerPredicate)
	if recordMarker == nil {
		t.Fatal("No Record State Marker")
	}
	if !Find(fourthDecisions, completeWorkflowPredicate) {
		t.Fatal("No CompleteWorkflow")
	}

}
