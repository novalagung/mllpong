package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- Test helpers ---

func startTestServer(t *testing.T, handler handlerFunc) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleConn(conn, handler)
		}
	}()
	return ln.Addr().String()
}

func sendAndReceive(t *testing.T, addr, msg string) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("connect to %s: %v", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(wrapMLLP([]byte(msg))); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw, err := readMLLP(bufio.NewReader(conn))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(raw)
}

func getMSA(resp string) (ackCode, ctrlID, text string) {
	for _, line := range strings.FieldsFunc(resp, func(r rune) bool { return r == '\r' || r == '\n' }) {
		if strings.HasPrefix(line, "MSA") {
			f := hl7Fields(line)
			return field(f, 1), field(f, 2), field(f, 3)
		}
	}
	return "", "", ""
}

func getERR(resp string) (errCode string, found bool) {
	for _, line := range strings.FieldsFunc(resp, func(r rune) bool { return r == '\r' || r == '\n' }) {
		if strings.HasPrefix(line, "ERR") {
			f := hl7Fields(line)
			return field(f, 3), true
		}
	}
	return "", false
}

func buildHL7(msgType, trigEvt string) string {
	now := time.Now().Format("20060102150405")
	var msh9 string
	if trigEvt != "" {
		msh9 = msgType + "^" + trigEvt + "^" + msgType + "_" + trigEvt
	} else {
		msh9 = msgType
	}
	return "MSH|^~\\&|SENDER|SENDFAC|RECEIVER|RECVFAC|" + now + "||" + msh9 + "|CTRL001|P|2.5\r"
}

func makeSmartHandler(rules []Rule) handlerFunc {
	return smartHandler(&RulesConfig{Rules: rules})
}

// --- Unit tests: messageType ---

func TestMessageType(t *testing.T) {
	cases := []struct{ input, want string }{
		{"ADT^A01^ADT_A01", "ADT"},
		{"ORM^O01^ORM_O01", "ORM"},
		{"ADT", "ADT"},
		{"", ""},
	}
	for _, c := range cases {
		if got := messageType(c.input); got != c.want {
			t.Errorf("messageType(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// --- Unit tests: triggerEvent ---

func TestTriggerEvent(t *testing.T) {
	cases := []struct{ input, want string }{
		{"ADT^A01^ADT_A01", "A01"},
		{"ORM^O01", "O01"},
		{"ADT", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := triggerEvent(c.input); got != c.want {
			t.Errorf("triggerEvent(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

// --- Unit tests: matchRule ---

func TestMatchRule_ExactMatch(t *testing.T) {
	rules := []Rule{
		{Match: "ADT^A01", Response: "AA"},
		{Match: "ADT", Response: "AE"},
		{Match: "*", Response: "AR"},
	}
	r := matchRule(rules, "ADT", "A01")
	if r == nil || r.Response != "AA" {
		t.Errorf("expected exact match AA, got %v", r)
	}
}

func TestMatchRule_MessageTypeOnly(t *testing.T) {
	rules := []Rule{
		{Match: "ADT^A08", Response: "AE"},
		{Match: "ADT", Response: "AA"},
		{Match: "*", Response: "AR"},
	}
	r := matchRule(rules, "ADT", "A01")
	if r == nil || r.Response != "AA" {
		t.Errorf("expected message-type-only match AA, got %v", r)
	}
}

func TestMatchRule_Wildcard(t *testing.T) {
	rules := []Rule{
		{Match: "ORM", Response: "AE"},
		{Match: "*", Response: "AA"},
	}
	r := matchRule(rules, "ORU", "R01")
	if r == nil || r.Response != "AA" {
		t.Errorf("expected wildcard match AA, got %v", r)
	}
}

func TestMatchRule_PriorityExactOverType(t *testing.T) {
	rules := []Rule{
		{Match: "ADT", Response: "AE"},
		{Match: "ADT^A01", Response: "AA"},
		{Match: "*", Response: "AR"},
	}
	r := matchRule(rules, "ADT", "A01")
	if r == nil || r.Response != "AA" {
		t.Errorf("exact match should beat type match, got %v", r)
	}
}

func TestMatchRule_PriorityTypeOverWildcard(t *testing.T) {
	rules := []Rule{
		{Match: "*", Response: "AR"},
		{Match: "ADT", Response: "AA"},
	}
	r := matchRule(rules, "ADT", "A99")
	if r == nil || r.Response != "AA" {
		t.Errorf("type match should beat wildcard, got %v", r)
	}
}

func TestMatchRule_NoMatch(t *testing.T) {
	rules := []Rule{{Match: "ORM", Response: "AE"}}
	if r := matchRule(rules, "ADT", "A01"); r != nil {
		t.Errorf("expected nil, got %v", r)
	}
}

func TestMatchRule_CaseInsensitive(t *testing.T) {
	rules := []Rule{{Match: "adt^a01", Response: "AA"}}
	r := matchRule(rules, "ADT", "A01")
	if r == nil || r.Response != "AA" {
		t.Errorf("expected case-insensitive match, got %v", r)
	}
}

func TestMatchRule_EmptyRules(t *testing.T) {
	if r := matchRule(nil, "ADT", "A01"); r != nil {
		t.Errorf("expected nil for empty rules, got %v", r)
	}
}

func TestMatchRule_EmptyTriggerEvent(t *testing.T) {
	rules := []Rule{{Match: "ADT", Response: "AA"}}
	r := matchRule(rules, "ADT", "")
	if r == nil || r.Response != "AA" {
		t.Errorf("expected ADT rule match, got %v", r)
	}
}

// --- Unit tests: loadRules ---

func TestLoadRules_ValidJSON(t *testing.T) {
	cfg := RulesConfig{Rules: []Rule{{Match: "ADT^A01", Response: "AA", AckText: "ok"}}}
	data, _ := json.Marshal(cfg)
	f := filepath.Join(t.TempDir(), "rules.json")
	os.WriteFile(f, data, 0644)

	loaded, _, err := loadRules(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(loaded.Rules) != 1 || loaded.Rules[0].Match != "ADT^A01" {
		t.Errorf("unexpected rules: %+v", loaded.Rules)
	}
}

func TestLoadRules_AllFields(t *testing.T) {
	raw := `{"rules":[{"match":"ADT^A01","response":"AE","error_code":100,"error_severity":"W","error_msg":"oops","delay_ms":50,"nack_rate":0.5,"ack_text":"hi"}]}`
	f := filepath.Join(t.TempDir(), "rules.json")
	os.WriteFile(f, []byte(raw), 0644)

	loaded, _, err := loadRules(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := loaded.Rules[0]
	if r.Response != "AE" || r.ErrorCode != 100 || r.ErrorSeverity != "W" ||
		r.ErrorMsg != "oops" || r.DelayMs != 50 || r.NackRate != 0.5 || r.AckText != "hi" {
		t.Errorf("field mismatch: %+v", r)
	}
}

func TestLoadRules_InvalidJSON(t *testing.T) {
	f := filepath.Join(t.TempDir(), "rules.json")
	os.WriteFile(f, []byte("not json"), 0644)
	if _, _, err := loadRules(f); err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestLoadRules_MissingFile(t *testing.T) {
	cfg, src, err := loadRules("/nonexistent/rules.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if src != "embedded" {
		t.Errorf("expected source %q, got %q", "embedded", src)
	}
	if len(cfg.Rules) == 0 {
		t.Error("expected embedded rules to be non-empty")
	}
}

func TestLoadRules_EmptyRules(t *testing.T) {
	f := filepath.Join(t.TempDir(), "rules.json")
	os.WriteFile(f, []byte(`{"rules":[]}`), 0644)
	loaded, _, err := loadRules(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(loaded.Rules) != 0 {
		t.Errorf("expected empty rules, got %d", len(loaded.Rules))
	}
}

// --- In-process tests: ackAllHandler ---

func TestAckAllHandler_ReturnsAA(t *testing.T) {
	addr := startTestServer(t, ackAllHandler)
	resp := sendAndReceive(t, addr, buildHL7("ADT", "A01"))
	if ack, _, _ := getMSA(resp); ack != "AA" {
		t.Errorf("expected AA, got %q", ack)
	}
}

func TestAckAllHandler_ReturnsAA_ForAnyMsgType(t *testing.T) {
	addr := startTestServer(t, ackAllHandler)
	for _, tc := range []struct{ msgType, trigEvt string }{
		{"ORM", "O01"}, {"ORU", "R01"}, {"SIU", "S12"}, {"MDM", "T01"}, {"MFN", ""},
	} {
		resp := sendAndReceive(t, addr, buildHL7(tc.msgType, tc.trigEvt))
		if ack, _, _ := getMSA(resp); ack != "AA" {
			t.Errorf("expected AA for %s^%s, got %q", tc.msgType, tc.trigEvt, ack)
		}
	}
}

func TestAckAllHandler_NoERRSegment(t *testing.T) {
	addr := startTestServer(t, ackAllHandler)
	resp := sendAndReceive(t, addr, buildHL7("ADT", "A01"))
	if _, found := getERR(resp); found {
		t.Error("ackAllHandler should not include ERR segment")
	}
}

// --- In-process tests: chaosHandler ---

func TestChaosHandler_ReturnsAR(t *testing.T) {
	addr := startTestServer(t, chaosHandler)
	resp := sendAndReceive(t, addr, buildHL7("ADT", "A01"))
	if ack, _, _ := getMSA(resp); ack != "AR" {
		t.Errorf("expected AR, got %q", ack)
	}
}

func TestChaosHandler_HasERRSegment(t *testing.T) {
	addr := startTestServer(t, chaosHandler)
	resp := sendAndReceive(t, addr, buildHL7("ADT", "A01"))
	if _, found := getERR(resp); !found {
		t.Error("expected ERR segment in chaos response")
	}
}

func TestChaosHandler_ReturnsAR_ForAnyMsgType(t *testing.T) {
	addr := startTestServer(t, chaosHandler)
	for _, tc := range []struct{ msgType, trigEvt string }{
		{"ORM", "O01"}, {"ORU", "R01"}, {"SIU", "S12"},
	} {
		resp := sendAndReceive(t, addr, buildHL7(tc.msgType, tc.trigEvt))
		if ack, _, _ := getMSA(resp); ack != "AR" {
			t.Errorf("expected AR for %s^%s, got %q", tc.msgType, tc.trigEvt, ack)
		}
	}
}

// --- In-process tests: smartHandler ---

func TestSmartHandler_ADT_A01_AA(t *testing.T) {
	addr := startTestServer(t, makeSmartHandler([]Rule{
		{Match: "ADT^A01", Response: "AA", AckText: "Patient admitted"},
	}))
	resp := sendAndReceive(t, addr, buildHL7("ADT", "A01"))
	if ack, _, text := getMSA(resp); ack != "AA" || text != "Patient admitted" {
		t.Errorf("got ack=%q text=%q", ack, text)
	}
}

func TestSmartHandler_ORM_AE(t *testing.T) {
	addr := startTestServer(t, makeSmartHandler([]Rule{
		{Match: "ORM", Response: "AE", ErrorCode: 207, ErrorSeverity: "E", ErrorMsg: "Order failed"},
	}))
	resp := sendAndReceive(t, addr, buildHL7("ORM", "O01"))
	if ack, _, _ := getMSA(resp); ack != "AE" {
		t.Errorf("expected AE, got %q", ack)
	}
	if errCode, found := getERR(resp); !found || errCode != "207" {
		t.Errorf("expected ERR 207, found=%v code=%q", found, errCode)
	}
}

func TestSmartHandler_ORM_AR(t *testing.T) {
	addr := startTestServer(t, makeSmartHandler([]Rule{
		{Match: "ORM", Response: "AR", ErrorCode: 207, ErrorSeverity: "F", ErrorMsg: "Order rejected"},
	}))
	resp := sendAndReceive(t, addr, buildHL7("ORM", "O01"))
	if ack, _, _ := getMSA(resp); ack != "AR" {
		t.Errorf("expected AR, got %q", ack)
	}
}

func TestSmartHandler_GenericTypeFallback(t *testing.T) {
	addr := startTestServer(t, makeSmartHandler([]Rule{
		{Match: "ADT^A01", Response: "AE"},
		{Match: "ADT", Response: "AA", AckText: "generic ADT"},
		{Match: "*", Response: "AR"},
	}))
	// ADT^A99 has no exact rule — falls to ADT type rule
	resp := sendAndReceive(t, addr, buildHL7("ADT", "A99"))
	if ack, _, _ := getMSA(resp); ack != "AA" {
		t.Errorf("expected AA for generic ADT fallback, got %q", ack)
	}
}

func TestSmartHandler_WildcardFallback(t *testing.T) {
	addr := startTestServer(t, makeSmartHandler([]Rule{
		{Match: "ADT", Response: "AE"},
		{Match: "*", Response: "AA"},
	}))
	resp := sendAndReceive(t, addr, buildHL7("ORU", "R01"))
	if ack, _, _ := getMSA(resp); ack != "AA" {
		t.Errorf("expected wildcard AA, got %q", ack)
	}
}

func TestSmartHandler_NackRate_AlwaysNACK(t *testing.T) {
	addr := startTestServer(t, makeSmartHandler([]Rule{
		{Match: "*", Response: "AA", NackRate: 1.0},
	}))
	for range 5 {
		resp := sendAndReceive(t, addr, buildHL7("ADT", "A01"))
		if ack, _, _ := getMSA(resp); ack != "AR" {
			t.Errorf("expected AR from nack_rate=1.0, got %q", ack)
		}
	}
}

func TestSmartHandler_NackRate_NeverNACK(t *testing.T) {
	addr := startTestServer(t, makeSmartHandler([]Rule{
		{Match: "*", Response: "AA", NackRate: 0.0},
	}))
	for range 5 {
		resp := sendAndReceive(t, addr, buildHL7("ADT", "A01"))
		if ack, _, _ := getMSA(resp); ack != "AA" {
			t.Errorf("expected AA from nack_rate=0.0, got %q", ack)
		}
	}
}

func TestSmartHandler_Delay(t *testing.T) {
	const delayMs = 150
	addr := startTestServer(t, makeSmartHandler([]Rule{
		{Match: "*", Response: "AA", DelayMs: delayMs},
	}))
	start := time.Now()
	sendAndReceive(t, addr, buildHL7("ADT", "A01"))
	if elapsed := time.Since(start); elapsed < time.Duration(delayMs)*time.Millisecond {
		t.Errorf("expected at least %dms delay, got %v", delayMs, elapsed)
	}
}

func TestSmartHandler_CustomAckText(t *testing.T) {
	const want = "All systems nominal"
	addr := startTestServer(t, makeSmartHandler([]Rule{
		{Match: "*", Response: "AA", AckText: want},
	}))
	resp := sendAndReceive(t, addr, buildHL7("ADT", "A01"))
	if _, _, text := getMSA(resp); text != want {
		t.Errorf("expected ack_text %q, got %q", want, text)
	}
}

func TestSmartHandler_CustomErrorCode(t *testing.T) {
	addr := startTestServer(t, makeSmartHandler([]Rule{
		{Match: "*", Response: "AE", ErrorCode: 100, ErrorSeverity: "W"},
	}))
	resp := sendAndReceive(t, addr, buildHL7("ADT", "A01"))
	if errCode, found := getERR(resp); !found || errCode != "100" {
		t.Errorf("expected ERR code 100, found=%v code=%q", found, errCode)
	}
}

func TestSmartHandler_DefaultErrorCode_207(t *testing.T) {
	addr := startTestServer(t, makeSmartHandler([]Rule{
		{Match: "*", Response: "AE"},
	}))
	resp := sendAndReceive(t, addr, buildHL7("ADT", "A01"))
	if errCode, found := getERR(resp); !found || errCode != "207" {
		t.Errorf("expected default ERR code 207, found=%v code=%q", found, errCode)
	}
}

func TestSmartHandler_AA_HasNoERRSegment(t *testing.T) {
	addr := startTestServer(t, makeSmartHandler([]Rule{{Match: "*", Response: "AA"}}))
	resp := sendAndReceive(t, addr, buildHL7("ADT", "A01"))
	if _, found := getERR(resp); found {
		t.Error("AA response should not contain ERR segment")
	}
}

func TestSmartHandler_NoMatchingRule_DefaultsToAA(t *testing.T) {
	addr := startTestServer(t, makeSmartHandler([]Rule{
		{Match: "ORM", Response: "AE"},
	}))
	resp := sendAndReceive(t, addr, buildHL7("ADT", "A01"))
	if ack, _, _ := getMSA(resp); ack != "AA" {
		t.Errorf("expected default AA for unmatched rule, got %q", ack)
	}
}

func TestSmartHandler_EmptyRules_DefaultsToAA(t *testing.T) {
	addr := startTestServer(t, makeSmartHandler(nil))
	resp := sendAndReceive(t, addr, buildHL7("ADT", "A01"))
	if ack, _, _ := getMSA(resp); ack != "AA" {
		t.Errorf("expected AA for empty rules, got %q", ack)
	}
}

func TestSmartHandler_BadMessage_LastDitchError(t *testing.T) {
	addr := startTestServer(t, makeSmartHandler([]Rule{{Match: "*", Response: "AA"}}))
	resp := sendAndReceive(t, addr, "EVN|A01|20240101\rPID|||123\r")
	if ack, _, _ := getMSA(resp); ack != "AR" {
		t.Errorf("expected AR from last-ditch error, got %q", ack)
	}
}

func TestSmartHandler_EchoesControlID(t *testing.T) {
	addr := startTestServer(t, makeSmartHandler([]Rule{{Match: "*", Response: "AA"}}))
	msg := "MSH|^~\\&|SENDER|FAC|RECV|FAC2|20240101120000||ADT^A01^ADT_A01|MYCTRLID|P|2.5\r"
	resp := sendAndReceive(t, addr, msg)
	if _, ctrlID, _ := getMSA(resp); ctrlID != "MYCTRLID" {
		t.Errorf("expected control ID echo %q, got %q", "MYCTRLID", ctrlID)
	}
}

func TestSmartHandler_MultipleMessagesOnOneConnection(t *testing.T) {
	addr := startTestServer(t, makeSmartHandler([]Rule{
		{Match: "ADT^A01", Response: "AA"},
		{Match: "ORM", Response: "AE", ErrorCode: 207},
		{Match: "ORU^R01", Response: "AA"},
	}))

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(conn)

	send := func(msg string) string {
		if _, err := conn.Write(wrapMLLP([]byte(msg))); err != nil {
			t.Fatalf("write: %v", err)
		}
		raw, err := readMLLP(reader)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		return string(raw)
	}

	if ack, _, _ := getMSA(send(buildHL7("ADT", "A01"))); ack != "AA" {
		t.Errorf("msg1 ADT^A01: expected AA")
	}
	if ack, _, _ := getMSA(send(buildHL7("ORM", "O01"))); ack != "AE" {
		t.Errorf("msg2 ORM^O01: expected AE")
	}
	if ack, _, _ := getMSA(send(buildHL7("ORU", "R01"))); ack != "AA" {
		t.Errorf("msg3 ORU^R01: expected AA")
	}
}

func TestSmartHandler_LoadedFromFile(t *testing.T) {
	raw := `{"rules":[
		{"match":"ADT^A01","response":"AA","ack_text":"from file"},
		{"match":"*","response":"AR"}
	]}`
	f := filepath.Join(t.TempDir(), "rules.json")
	os.WriteFile(f, []byte(raw), 0644)

	cfg, _, err := loadRules(f)
	if err != nil {
		t.Fatalf("loadRules: %v", err)
	}
	addr := startTestServer(t, smartHandler(cfg))

	resp := sendAndReceive(t, addr, buildHL7("ADT", "A01"))
	if ack, _, text := getMSA(resp); ack != "AA" || text != "from file" {
		t.Errorf("expected AA/from file, got ack=%q text=%q", ack, text)
	}

	resp2 := sendAndReceive(t, addr, buildHL7("ORU", "R01"))
	if ack, _, _ := getMSA(resp2); ack != "AR" {
		t.Errorf("expected AR from wildcard, got %q", ack)
	}
}

// --- Unit tests: envOr ---

func TestEnvOr_ReturnsEnvValue(t *testing.T) {
	t.Setenv("TEST_ENVOR_KEY", "fromenv")
	if got := envOr("TEST_ENVOR_KEY", "fallback"); got != "fromenv" {
		t.Errorf("expected %q, got %q", "fromenv", got)
	}
}

func TestEnvOr_ReturnsFallback(t *testing.T) {
	os.Unsetenv("TEST_ENVOR_KEY_MISSING")
	if got := envOr("TEST_ENVOR_KEY_MISSING", "fallback"); got != "fallback" {
		t.Errorf("expected %q, got %q", "fallback", got)
	}
}

// --- Unit tests: readMLLP false end marker ---

func TestReadMLLP_FalseEndMarker(t *testing.T) {
	// 0x1C not followed by 0x0D must be written into the buffer as content, not treated as end.
	raw := []byte{mllpStart, 'h', 'i', mllpEnd1, 'A', mllpEnd1, mllpEnd2}
	got, err := readMLLP(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "hi\x1cA"
	if string(got) != want {
		t.Errorf("expected %q, got %q", want, string(got))
	}
}

// --- In-process tests: handler parse error paths ---

func TestAckAllHandler_BadMessage_LastDitchError(t *testing.T) {
	addr := startTestServer(t, ackAllHandler)
	resp := sendAndReceive(t, addr, "EVN|A01|20240101\rPID|||123\r")
	if ack, _, _ := getMSA(resp); ack != "AR" {
		t.Errorf("expected AR from last-ditch error, got %q", ack)
	}
}

func TestChaosHandler_BadMessage_LastDitchError(t *testing.T) {
	addr := startTestServer(t, chaosHandler)
	resp := sendAndReceive(t, addr, "EVN|A01|20240101\rPID|||123\r")
	if ack, _, _ := getMSA(resp); ack != "AR" {
		t.Errorf("expected AR from last-ditch error, got %q", ack)
	}
}

func TestSmartHandler_EmptyResponse_DefaultsToAA(t *testing.T) {
	addr := startTestServer(t, makeSmartHandler([]Rule{{Match: "*", Response: ""}}))
	resp := sendAndReceive(t, addr, buildHL7("ADT", "A01"))
	if ack, _, _ := getMSA(resp); ack != "AA" {
		t.Errorf("expected AA for empty response field, got %q", ack)
	}
}

// --- In-process tests: handleConn error paths ---

func TestHandleConn_NonEOFReadError(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	// Deadline triggers a non-EOF read error while readMLLP waits for more bytes.
	server.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleConn(server, ackAllHandler)
	}()
	client.Write([]byte{mllpStart}) // start byte only — never completes
	<-done
}

func TestHandleConn_WriteError(t *testing.T) {
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleConn(server, ackAllHandler)
	}()
	// Write a complete message then immediately close so the response write fails.
	client.Write(wrapMLLP([]byte(buildHL7("ADT", "A01"))))
	client.Close()
	<-done
}

func TestHandleConn_EOFOnEmptyRead(t *testing.T) {
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handleConn(server, ackAllHandler)
	}()
	client.Close() // EOF with no data — should return silently
	<-done
}

// --- In-process tests: serve ---

func TestServe_AcceptsAndResponds(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go serve(ln, ackAllHandler, &wg)
	t.Cleanup(func() { ln.Close(); wg.Wait() })

	resp := sendAndReceive(t, ln.Addr().String(), buildHL7("ADT", "A01"))
	if ack, _, _ := getMSA(resp); ack != "AA" {
		t.Errorf("expected AA, got %q", ack)
	}
}

func TestServe_StopsOnListenerClose(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go serve(ln, ackAllHandler, &wg)
	ln.Close()
	wg.Wait() // returns only if serve exited after the listener closed
}

// --- In-process tests: run ---

func TestRun_StartsAllThreeHandlersAndSaves(t *testing.T) {
	ackLn, _ := net.Listen("tcp", "127.0.0.1:0")
	chaosLn, _ := net.Listen("tcp", "127.0.0.1:0")
	smartLn, _ := net.Listen("tcp", "127.0.0.1:0")
	ackAddr := ackLn.Addr().String()
	chaosAddr := chaosLn.Addr().String()
	smartAddr := smartLn.Addr().String()
	ackPort := ackLn.Addr().(*net.TCPAddr).Port
	chaosPort := chaosLn.Addr().(*net.TCPAddr).Port
	smartPort := smartLn.Addr().(*net.TCPAddr).Port
	// Release ports so run can bind them.
	ackLn.Close()
	chaosLn.Close()
	smartLn.Close()

	saveDir := filepath.Join(t.TempDir(), "files")

	go run("127.0.0.1",
		fmt.Sprintf("%d", ackPort),
		fmt.Sprintf("%d", chaosPort),
		fmt.Sprintf("%d", smartPort),
		"/nonexistent/rules.json",
		saveDir,
	)

	waitReady := func(addr string) {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond); err == nil {
				c.Close()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("server at %s not ready within 2s", addr)
	}
	waitReady(ackAddr)
	waitReady(chaosAddr)
	waitReady(smartAddr)

	if ack, _, _ := getMSA(sendAndReceive(t, ackAddr, buildHL7("ADT", "A01"))); ack != "AA" {
		t.Errorf("ack handler: expected AA, got %q", ack)
	}
	if ack, _, _ := getMSA(sendAndReceive(t, chaosAddr, buildHL7("ADT", "A01"))); ack != "AR" {
		t.Errorf("chaos handler: expected AR, got %q", ack)
	}
	if ack, _, _ := getMSA(sendAndReceive(t, smartAddr, buildHL7("ADT", "A01"))); ack != "AA" {
		t.Errorf("smart handler: expected AA, got %q", ack)
	}

	entries, err := os.ReadDir(saveDir)
	if err != nil {
		t.Fatalf("read save dir: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("expected all 3 handlers to save their message, got %d files", len(entries))
	}
}

// --- Unit tests: readMLLP EOF paths ---

func TestReadMLLP_EOFBeforeStart(t *testing.T) {
	_, err := readMLLP(bufio.NewReader(bytes.NewReader(nil)))
	if err != io.EOF {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

func TestReadMLLP_EOFAfterStart(t *testing.T) {
	_, err := readMLLP(bufio.NewReader(bytes.NewReader([]byte{mllpStart})))
	if err != io.EOF {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

func TestReadMLLP_EOFAfterFalseEnd(t *testing.T) {
	// EOF while reading the byte after 0x1C (before we know if it's the end marker).
	raw := []byte{mllpStart, 'h', mllpEnd1}
	_, err := readMLLP(bufio.NewReader(bytes.NewReader(raw)))
	if err != io.EOF {
		t.Errorf("expected io.EOF, got %v", err)
	}
}

// --- Unit tests: safeFilePart ---

func TestSafeFilePart(t *testing.T) {
	cases := []struct{ input, want string }{
		{"ADT-A01", "ADT-A01"},
		{"MSG001", "MSG001"},
		{"", "unknown"},
		{"../../etc/passwd", "______etc_passwd"},
		{"..", "__"},
		{"a/b\\c:d*e?f", "a_b_c_d_e_f"},
		{"ctrl id", "ctrl_id"},
		{"2.5", "2_5"},
	}
	for _, c := range cases {
		if got := safeFilePart(c.input); got != c.want {
			t.Errorf("safeFilePart(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestSafeFilePart_Truncates(t *testing.T) {
	got := safeFilePart(strings.Repeat("A", 200))
	if len(got) != 64 {
		t.Errorf("expected 64 chars, got %d", len(got))
	}
}

// --- Unit tests: saveMessage ---

func TestSaveMessage_WritesRawMessage(t *testing.T) {
	dir := t.TempDir()
	msg := buildHL7("ADT", "A01")

	path, err := saveMessage(dir, msg)
	if err != nil {
		t.Fatalf("saveMessage: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != msg {
		t.Errorf("saved content = %q, want %q", got, msg)
	}
}

func TestSaveMessage_FileNameCarriesTypeAndControlID(t *testing.T) {
	dir := t.TempDir()

	path, err := saveMessage(dir, buildHL7("ORU", "R01"))
	if err != nil {
		t.Fatalf("saveMessage: %v", err)
	}

	name := filepath.Base(path)
	if !strings.Contains(name, "ORU-R01") {
		t.Errorf("file name %q missing message type", name)
	}
	if !strings.Contains(name, "CTRL001") {
		t.Errorf("file name %q missing control ID", name)
	}
	if !strings.HasSuffix(name, ".hl7") {
		t.Errorf("file name %q missing .hl7 extension", name)
	}
}

func TestSaveMessage_UnparsableMessage(t *testing.T) {
	dir := t.TempDir()

	path, err := saveMessage(dir, "not an hl7 message")
	if err != nil {
		t.Fatalf("saveMessage: %v", err)
	}

	if name := filepath.Base(path); !strings.Contains(name, "unknown") {
		t.Errorf("file name %q should mark the message as unknown", name)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "not an hl7 message" {
		t.Errorf("saved content = %q, want the raw message", got)
	}
}

func TestSaveMessage_IdenticalMessagesDoNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	msg := buildHL7("ADT", "A01")

	first, err := saveMessage(dir, msg)
	if err != nil {
		t.Fatalf("saveMessage: %v", err)
	}
	second, err := saveMessage(dir, msg)
	if err != nil {
		t.Fatalf("saveMessage: %v", err)
	}

	if first == second {
		t.Fatalf("both messages landed in %q", first)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 files, got %d", len(entries))
	}
}

func TestSaveMessage_ConcurrentSavesAreUnique(t *testing.T) {
	dir := t.TempDir()
	msg := buildHL7("ADT", "A01")

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := saveMessage(dir, msg); err != nil {
				t.Errorf("saveMessage: %v", err)
			}
		}()
	}
	wg.Wait()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 50 {
		t.Errorf("expected 50 files, got %d", len(entries))
	}
}

func TestSaveMessage_ControlIDCannotEscapeDir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "files")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	msg := "MSH|^~\\&|S|SF|R|RF|20240101120000||ADT^A01^ADT_A01|../../escaped|P|2.5\r"

	path, err := saveMessage(dir, msg)
	if err != nil {
		t.Fatalf("saveMessage: %v", err)
	}

	if filepath.Dir(path) != dir {
		t.Errorf("message escaped to %q, want it inside %q", path, dir)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read root: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("expected only the files dir in root, got %d entries", len(entries))
	}
}

func TestSaveMessage_MissingDirReturnsError(t *testing.T) {
	_, err := saveMessage(filepath.Join(t.TempDir(), "nope"), buildHL7("ADT", "A01"))
	if err == nil {
		t.Error("expected an error when the save dir does not exist")
	}
}

// --- Unit tests: withSaver ---

func TestWithSaver_EmptyPathDisablesSaving(t *testing.T) {
	dir := t.TempDir()

	handler := withSaver("", ackAllHandler)
	if ack, _, _ := getMSA(string(handler(buildHL7("ADT", "A01")))); ack != "AA" {
		t.Errorf("expected AA, got %q", ack)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected no files, got %d", len(entries))
	}
}

func TestWithSaver_SavesForEveryHandler(t *testing.T) {
	handlers := map[string]handlerFunc{
		"ack":   ackAllHandler,
		"chaos": chaosHandler,
		"smart": makeSmartHandler([]Rule{{Match: "*", Response: "AA"}}),
	}
	for name, h := range handlers {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			msg := buildHL7("ADT", "A01")

			resp := withSaver(dir, h)(msg)
			if _, ctrlID, _ := getMSA(string(resp)); ctrlID != "CTRL001" {
				t.Errorf("handler did not answer the message, got ctrl-id %q", ctrlID)
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("read dir: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("expected 1 saved file, got %d", len(entries))
			}
			got, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if string(got) != msg {
				t.Errorf("saved content = %q, want %q", got, msg)
			}
		})
	}
}

func TestWithSaver_StillAnswersWhenSaveFails(t *testing.T) {
	handler := withSaver(filepath.Join(t.TempDir(), "nope"), ackAllHandler)
	if ack, _, _ := getMSA(string(handler(buildHL7("ADT", "A01")))); ack != "AA" {
		t.Errorf("expected AA despite the save failure, got %q", ack)
	}
}

// --- In-process tests: withSaver ---

func TestWithSaver_SavesOverTheWire(t *testing.T) {
	dir := t.TempDir()
	addr := startTestServer(t, withSaver(dir, ackAllHandler))

	sendAndReceive(t, addr, buildHL7("ADT", "A01"))
	sendAndReceive(t, addr, buildHL7("ORU", "R01"))

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 saved files, got %d", len(entries))
	}
}
