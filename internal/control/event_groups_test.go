package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/imksoo/routerd/pkg/api"
	"github.com/imksoo/routerd/pkg/eventd"
	"github.com/imksoo/routerd/pkg/federation"
	routerstate "github.com/imksoo/routerd/pkg/state"
	"github.com/imksoo/routerd2/internal/events"
	"github.com/imksoo/routerd2/internal/localapi"
	"github.com/imksoo/routerd2/internal/protocol/eventgroup"
	"github.com/imksoo/routerd2/internal/protocol/eventgroupconfig"
	"github.com/imksoo/routerd2/internal/resourceprogram"
	"github.com/imksoo/routerd2/internal/serviceconfig"
)

func TestEventGroupWithIndependentRouterd(t *testing.T) {
	binary := os.Getenv("OLD_EVENTD")
	if binary == "" {
		t.Skip("set OLD_EVENTD to an independent routerd-eventd executable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e, _ := fixture(t)
	journal, err := events.Open(events.Options{Directory: filepath.Join(e.store.Dir, "events"), MaxJobs: 2})
	if err != nil {
		t.Fatal(err)
	}
	e.journal = journal
	t.Cleanup(func() { journal.Close() })
	maintenance, stopMaintenance := context.WithCancel(ctx)
	maintenanceDone := make(chan struct{})
	go func() { defer close(maintenanceDone); e.monitorEventStore(maintenance) }()
	defer func() { stopMaintenance(); <-maintenanceDone }()
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "secret")
	if err := os.WriteFile(secretFile, []byte("fixture-event-key"), 0600); err != nil {
		t.Fatal(err)
	}
	address := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		v := l.Addr().String()
		l.Close()
		return v
	}
	nativeAddress, oldAddress := address(), address()
	cfg := eventgroupconfig.Config{Group: "fixture", NodeName: "native", Listen: netip.MustParseAddrPort(nativeAddress), SecretFile: secretFile, ReplayWindow: 5 * time.Minute, MaxEvents: 2, MaxAge: 24 * time.Hour, Peers: []eventgroupconfig.Peer{{NodeName: "old", Endpoint: "http://" + oldAddress}, {NodeName: "unreachable", Endpoint: "http://" + address()}}}
	req := request(e, "enable-event-group")
	req.Config.Resources = []resourceprogram.Resource{{ID: "service/federation", Service: &serviceconfig.Service{Name: "federation", EventGroup: &cfg}}, {ID: "received", EventRule: &api.EventRuleSpec{Pattern: api.EventRulePatternSpec{Operator: "count", Topic: "fixture.observed", Threshold: 1}, Emit: api.EventRuleEmitSpec{Topic: "fixture.evaluated"}}}}
	if _, err := e.Commit(ctx, req); err != nil {
		t.Fatal(err)
	}
	processRules := func() {
		deadline := time.Now().Add(6 * time.Second)
		for {
			err := e.ProcessEventRules(ctx, time.Now())
			if err == nil {
				return
			}
			if !errors.Is(err, events.ErrCapacity) || time.Now().After(deadline) {
				t.Fatal(err)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	processRules()
	ready := make(chan struct{})
	controlDone := make(chan error, 1)
	controlCtx, controlCancel := context.WithCancel(ctx)
	go func() {
		controlDone <- localapi.Serve(controlCtx, filepath.Join(dir, "control.sock"), e.Handler(), func() { close(ready) })
	}()
	<-ready
	defer func() {
		controlCancel()
		if err := <-controlDone; err != nil {
			t.Error(err)
		}
	}()
	local := localapi.NewClient(filepath.Join(dir, "control.sock"))
	startNative := func() func() {
		child, stop := context.WithCancel(ctx)
		ready := make(chan struct{})
		done := make(chan error, 1)
		go func() { done <- eventgroup.Run(child, cfg, dir, "service.federation", func() { close(ready) }) }()
		select {
		case <-ready:
		case err := <-done:
			t.Fatal(err)
		case <-time.After(5 * time.Second):
			t.Fatal("native event transport did not start")
		}
		return func() {
			stop()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("native event transport did not stop")
			}
		}
	}
	stopNative := startNative()
	defer func() {
		if stopNative != nil {
			stopNative()
		}
	}()
	oldState := filepath.Join(dir, "old.db")
	oldCfg := eventd.Config{NodeName: "old", Group: "fixture", Listen: eventd.Listen{Address: "127.0.0.1", Port: int(netip.MustParseAddrPort(oldAddress).Port())}, SecretFile: secretFile, StatePath: oldState, Peers: []eventd.PeerConfig{{NodeName: "native", Endpoint: "http://" + nativeAddress}}, PushInterval: 100 * time.Millisecond}
	oldCfg.ApplyDefaults()
	body, err := eventd.MarshalConfigJSON(oldCfg)
	if err != nil {
		t.Fatal(err)
	}
	oldConfig := filepath.Join(dir, "old.json")
	if err := os.WriteFile(oldConfig, body, 0600); err != nil {
		t.Fatal(err)
	}
	startOld := func() func() {
		child, stop := context.WithCancel(ctx)
		command := exec.CommandContext(child, binary, "daemon", "--config-file", oldConfig)
		log, err := os.Create(filepath.Join(dir, "old.log"))
		if err != nil {
			t.Fatal(err)
		}
		command.Stdout, command.Stderr = log, log
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		until := time.Now().Add(5 * time.Second)
		for {
			response, err := http.Get("http://" + oldAddress + "/v1/status")
			if err == nil {
				response.Body.Close()
				break
			}
			if time.Now().After(until) {
				stop()
				<-done
				log.Close()
				data, _ := os.ReadFile(log.Name())
				t.Fatal("old eventd did not listen", string(data))
			}
			time.Sleep(20 * time.Millisecond)
		}
		return func() { stop(); <-done; log.Close() }
	}
	stopOld := startOld()
	defer func() {
		if stopOld != nil {
			stopOld()
		}
	}()
	waitOld := func(id string) {
		until := time.Now().Add(15 * time.Second)
		for {
			store, err := routerstate.OpenSQLiteReadOnly(oldState)
			if err != nil {
				t.Fatal(err)
			}
			records, err := store.ListFederationEvents("fixture", true, time.Now().Unix())
			store.Close()
			if err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				if record.ID == id {
					return
				}
			}
			if time.Now().After(until) {
				t.Fatal("old peer did not receive", id)
			}
			time.Sleep(30 * time.Millisecond)
		}
	}
	emit := func(id string, expiry time.Time) {
		event := federation.Event{ID: id, Type: "fixture.local", Subject: "192.0.2.7/32", Payload: map[string]string{"proof": "native"}, ObservedAt: time.Now().UTC(), ExpiresAt: expiry}
		if err := local.Do(ctx, "POST", "/v1/event-groups/service.federation/emit", event, nil); err != nil {
			t.Fatal(err)
		}
	}
	emit("native-first", time.Time{})
	waitOld("native-first")
	// The unchanged old daemon consumes its real durable outbox. This is the
	// same local observation input used by routerctl federation emit.
	oldStore, err := routerstate.OpenSQLite(oldState)
	if err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC().Truncate(time.Second)
	expires := observed.Add(time.Minute)
	incoming := routerstate.EventRecord{ID: "old-first", Group: "fixture", SourceNode: "old", Type: "fixture.observed", Subject: "198.51.100.7/32", DedupeKey: "observed-address", Payload: map[string]string{"proof": "old"}, ObservedAt: observed, ExpiresAt: expires}
	if err := oldStore.RecordFederationEvent(incoming); err != nil {
		t.Fatal(err)
	}
	oldStore.Close()
	until := time.Now().Add(5 * time.Second)
	for {
		rows, err := journal.List(ctx, 0, 64)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, row := range rows {
			if f := row.Event.Federation; f != nil && f.ID == incoming.ID {
				if f.SourceNode != "old" || f.Subject != incoming.Subject || row.Event.Attributes["proof"] != "old" || !row.Event.ExpiresAt.Equal(expires) {
					t.Fatal("wire fields or original expiry were lost")
				}
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(until) {
			t.Fatal("old daemon did not deliver its observation")
		}
		time.Sleep(20 * time.Millisecond)
	}
	processRules()
	rows, err := journal.List(ctx, 0, 64)
	if err != nil {
		t.Fatal(err)
	}
	evaluated := false
	for _, row := range rows {
		if row.Event.Topic == "fixture.evaluated" {
			evaluated = true
		}
	}
	if !evaluated {
		t.Fatal("received event did not enter the existing EventRule path")
	}
	signed := func(event federation.Event, at time.Time, key string) int {
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest("POST", "http://"+nativeAddress+"/v1/events", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("X-Routerd-Timestamp", strconv.FormatInt(at.Unix(), 10))
		request.Header.Set("X-Routerd-Signature", federation.Sign([]byte(key), at.Unix(), data))
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response.StatusCode
	}
	remote := federation.Event{ID: incoming.ID, Group: incoming.Group, SourceNode: incoming.SourceNode, Type: incoming.Type, Subject: incoming.Subject, DedupeKey: incoming.DedupeKey, Payload: incoming.Payload, ObservedAt: incoming.ObservedAt, ExpiresAt: incoming.ExpiresAt}
	if code := signed(remote, time.Now().Add(-6*time.Minute), "fixture-event-key"); code != 403 {
		t.Fatal("stale signed replay accepted", code)
	}
	if code := signed(remote, time.Now(), "wrong-key"); code != 401 {
		t.Fatal("incorrect signature accepted", code)
	}
	if code := signed(remote, time.Now(), "fixture-event-key"); code != 200 {
		t.Fatal("idempotent resend refused", code)
	}
	rows, _ = journal.List(ctx, 0, 64)
	copies := 0
	for _, row := range rows {
		if f := row.Event.Federation; f != nil && f.ID == incoming.ID {
			copies++
		}
	}
	if copies != 1 {
		t.Fatal("duplicate remote history", copies)
	}
	stopOld()
	stopOld = nil
	emit("expired-during-outage", time.Now().Add(300*time.Millisecond))
	emit("native-recovery", time.Now().Add(time.Minute))
	emit("native-later", time.Now().Add(time.Minute))
	stopNative()
	stopNative = nil
	time.Sleep(350 * time.Millisecond)
	stopNative = startNative()
	// Both delivery slots are occupied while the old peer is down. The
	// unchanged peer's return must drain them without removing configuration.
	// Allow the existing ten-second delivery tick after compaction releases a slot.
	deadline := time.Now().Add(15 * time.Second)
	for {
		pending, err := journal.Pending(ctx, eventgroupconfig.Consumer("service.federation", "old"), 8)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("outage did not retain queued work")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stopOld = startOld()
	waitOld("native-recovery")
	waitOld("native-later")
	oldStore, err = routerstate.OpenSQLiteReadOnly(oldState)
	if err != nil {
		t.Fatal(err)
	}
	oldRows, err := oldStore.ListFederationEvents("fixture", true, time.Now().Unix())
	oldStore.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range oldRows {
		if row.ID == "expired-during-outage" {
			t.Fatal("restart replayed an expired fact")
		}
	}
	// Once consumers have acknowledged, group retention uses the existing
	// journal deletion path and leaves unrelated local history alone.
	processRules()
	// A failed peer revisits completed delivery work on its ten-second tick.
	until = time.Now().Add(15 * time.Second)
	for {
		if err := e.maintainEventStore(ctx, compactEventWork); err != nil {
			t.Fatal(err)
		}
		if err := local.Do(ctx, "POST", "/v1/event-groups/service.federation/prune", nil, nil); err != nil {
			t.Fatal(err)
		}
		rows, _ = journal.List(ctx, 0, 64)
		count := 0
		for _, row := range rows {
			if row.Event.Federation != nil {
				count++
			}
		}
		if count <= cfg.MaxEvents {
			break
		}
		if time.Now().After(until) {
			t.Fatal("configured group retention did not run", count)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Log("independent routerd-eventd exchanged signed events with the native service; journal/rule delivery, duplicate suppression, stale signature rejection, restart recovery, expiry and scoped retention and full-queue recovery passed")
}

func TestEventGroupCapacityRecoversAfterPeerRemoval(t *testing.T) {
	ctx := context.Background()
	e, _ := fixture(t)
	journal := attachJournal(t, e, 2)
	cfg := eventgroupconfig.Config{Group: "capacity", NodeName: "local", SecretFile: "/etc/routerd/event-key", ReplayWindow: time.Minute, MaxEvents: 1, Peers: []eventgroupconfig.Peer{{NodeName: "removed", Endpoint: "http://127.0.0.1:1"}}}
	req := request(e, "configure-group")
	req.Config.Resources = []resourceprogram.Resource{{ID: "service/group", Service: &serviceconfig.Service{Name: "group", EventGroup: &cfg}}}
	if _, err := e.Commit(ctx, req); err != nil {
		t.Fatal(err)
	}
	call := func(method, path string, body []byte) int {
		w := httptest.NewRecorder()
		e.Handler().ServeHTTP(w, httptest.NewRequest(method, path, bytes.NewReader(body)))
		return w.Code
	}
	emit := func(id string) int {
		data, err := json.Marshal(federation.Event{ID: id, Type: "fixture.local", ObservedAt: time.Now().UTC()})
		if err != nil {
			t.Fatal(err)
		}
		return call("POST", "/v1/event-groups/service.group/emit", data)
	}
	if emit("one") != 200 || emit("two") != 200 {
		t.Fatal("initial durable input rejected")
	}
	if code := call("GET", "/v1/event-groups/service.group/outbox/removed", nil); code != 200 {
		t.Fatal("could not schedule actual deliveries", code)
	}
	if emit("three") != 503 {
		t.Fatal("full journal acknowledged an event it could not persist")
	}
	if code := call("GET", "/v1/version", nil); code != 200 {
		t.Fatal("history capacity blocked independent control access", code)
	}
	pending, err := journal.Pending(ctx, eventgroupconfig.Consumer("service.group", "removed"), 8)
	if err != nil || len(pending) != 2 {
		t.Fatal("capacity failure lost pending deliveries", len(pending), err)
	}
	req = request(e, "remove-peer")
	cfg.Peers = nil
	req.Config.Resources = []resourceprogram.Resource{{ID: "service/group", Service: &serviceconfig.Service{Name: "group", EventGroup: &cfg}}}
	if _, err := e.Commit(ctx, req); err != nil {
		t.Fatal(err)
	}
	// One maintenance pass marks/compacts retired work. The next removes
	// the cursor after the existing outbox foreign-key dependency is gone.
	for range 2 {
		if err := e.maintainEventStore(ctx, compactEventWork); err != nil {
			t.Fatal(err)
		}
	}
	if code := call("POST", "/v1/event-groups/service.group/prune", nil); code != 200 {
		t.Fatal("group retention failed", code)
	}
	if emit("three") != 200 {
		t.Fatal("removed peer retained history capacity")
	}
	rows, err := journal.List(ctx, 0, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Event.Federation.ID != "two" || rows[1].Event.Federation.ID != "three" {
		t.Fatal("retirement removed the wrong history", rows)
	}
}

func TestEventGroupReadditionSerializesRegistrationWithRetirement(t *testing.T) {
	ctx := context.Background()
	e, _ := fixture(t)
	journal := attachJournal(t, e, 0)
	cfg := eventgroupconfig.Config{Group: "readd", NodeName: "local", SecretFile: "/etc/routerd/event-key", ReplayWindow: time.Minute, Peers: []eventgroupconfig.Peer{{NodeName: "peer", Endpoint: "http://127.0.0.1:1"}}}
	declaration := resourceprogram.Resource{ID: "service/group", Service: &serviceconfig.Service{Name: "group", EventGroup: &cfg}}
	req := request(e, "initial")
	req.Config.Resources = []resourceprogram.Resource{declaration}
	if _, err := e.Commit(ctx, req); err != nil {
		t.Fatal(err)
	}
	consumer := eventgroupconfig.Consumer("service.group", "peer")
	if _, err := journal.Cursor(ctx, consumer); err != nil {
		t.Fatal(err)
	}
	req = request(e, "remove")
	if _, err := e.Commit(ctx, req); err != nil {
		t.Fatal(err)
	}
	// Hold the same feature-specific section as retirement after it observed
	// the removed configuration. Config commits must still make progress.
	e.eventGroupMu.Lock()
	req = request(e, "readd")
	req.Config.Resources = []resourceprogram.Resource{declaration}
	if _, err := e.Commit(ctx, req); err != nil {
		e.eventGroupMu.Unlock()
		t.Fatal(err)
	}
	started, done := make(chan struct{}), make(chan int, 1)
	go func() {
		close(started)
		w := httptest.NewRecorder()
		e.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/event-groups/service.group/emit", bytes.NewBufferString(`{"id":"after-readd","type":"fixture.local"}`)))
		done <- w.Code
	}()
	<-started
	// This is the actual old-configuration SQL retirement, not a test store.
	if err := journal.RetireFederationPeers(ctx, nil); err != nil {
		e.eventGroupMu.Unlock()
		t.Fatal(err)
	}
	select {
	case code := <-done:
		e.eventGroupMu.Unlock()
		t.Fatal("registration crossed the retirement section", code)
	default:
	}
	e.eventGroupMu.Unlock()
	if code := <-done; code != 200 {
		t.Fatal("readded group did not accept its event", code)
	}
	w := httptest.NewRecorder()
	e.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/event-groups/service.group/outbox/peer", nil))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var page eventgroupconfig.Page
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Deliveries) != 1 || page.Deliveries[0].Event.ID != "after-readd" {
		t.Fatal("old retirement lost the readded group's accepted event", page)
	}
}
