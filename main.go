package main

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed rules.json
var defaultRules []byte

const (
	mllpStart = byte(0x0B)
	mllpEnd1  = byte(0x1C)
	mllpEnd2  = byte(0x0D)
)

// hl7Fields splits an HL7 segment string by "|" into a 0-indexed slice.
// Index 0 is the segment name, index 1 is MSH.2, index N is MSH.(N+1).
func hl7Fields(segment string) []string {
	return strings.Split(segment, "|")
}

func field(fields []string, i int) string {
	if i < len(fields) {
		return fields[i]
	}
	return ""
}

// messageType extracts MSH.9.1 (e.g. "ADT" from "ADT^A01^ADT_A01").
func messageType(msh9 string) string {
	parts := strings.SplitN(msh9, "^", 2)
	return parts[0]
}

// triggerEvent extracts MSH.9.2 (e.g. "A01" from "ADT^A01^ADT_A01").
func triggerEvent(msh9 string) string {
	parts := strings.SplitN(msh9, "^", 3)
	if len(parts) >= 2 {
		return parts[1]
	}
	return ""
}

func parseMSH(raw string) ([]string, error) {
	lines := strings.FieldsFunc(raw, func(r rune) bool { return r == '\r' || r == '\n' })
	for _, line := range lines {
		if strings.HasPrefix(line, "MSH") {
			return hl7Fields(line), nil
		}
	}
	return nil, fmt.Errorf("no MSH segment found")
}

func timestamp() string {
	return time.Now().Format("20060102150405")
}

func wrapMLLP(msg []byte) []byte {
	var buf bytes.Buffer
	buf.WriteByte(mllpStart)
	buf.Write(msg)
	buf.WriteByte(mllpEnd1)
	buf.WriteByte(mllpEnd2)
	return buf.Bytes()
}

func readMLLP(r *bufio.Reader) ([]byte, error) {
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == mllpStart {
			break
		}
	}

	var buf bytes.Buffer
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == mllpEnd1 {
			next, err := r.ReadByte()
			if err != nil {
				return nil, err
			}
			if next == mllpEnd2 {
				break
			}
			buf.WriteByte(b)
			buf.WriteByte(next)
		} else {
			buf.WriteByte(b)
		}
	}
	return buf.Bytes(), nil
}

func buildACK(msh []string) []byte {
	ctrlID := field(msh, 9)
	procID := field(msh, 10)
	version := field(msh, 11)
	msgType := fmt.Sprintf("ACK^%s^ACK", triggerEvent(field(msh, 8)))

	var sb strings.Builder
	fmt.Fprintf(&sb, "MSH|^~\\&|mllpong|mllpong|%s|%s|%s||%s|%s|%s|%s\r",
		field(msh, 2), field(msh, 3), timestamp(), msgType, ctrlID, procID, version)
	fmt.Fprintf(&sb, "MSA|AA|%s|Wow, such message, very valid, Wow!\r", ctrlID)
	return wrapMLLP([]byte(sb.String()))
}

func buildNACK(msh []string, errCode int, severity, errMsg string) []byte {
	ctrlID := field(msh, 9)
	procID := field(msh, 10)
	version := field(msh, 11)
	msgType := fmt.Sprintf("ACK^%s^ACK", triggerEvent(field(msh, 8)))

	var sb strings.Builder
	fmt.Fprintf(&sb, "MSH|^~\\&|mllpong|mllpong|%s|%s|%s||%s|%s|%s|%s\r",
		field(msh, 2), field(msh, 3), timestamp(), msgType, ctrlID, procID, version)
	fmt.Fprintf(&sb, "MSA|AR|%s\r", ctrlID)
	fmt.Fprintf(&sb, "ERR|||%d||%s||%s\r", errCode, severity, errMsg)
	return wrapMLLP([]byte(sb.String()))
}

func buildLastDitchError(exc error) []byte {
	var sb strings.Builder
	fmt.Fprintf(&sb, "MSH|^~\\&|mllpong|mllpong|||%s||ACK^^ACK|||P|2.5\r", timestamp())
	sb.WriteString("MSA|AR|\r")
	fmt.Fprintf(&sb, "ERR|||207||F||Cannot create valid error resp without a valid request header! %s\r", exc)
	return wrapMLLP([]byte(sb.String()))
}

// Rule defines how the smart handler responds to a specific message type.
type Rule struct {
	Match         string  `json:"match"`          // "ADT^A01", "ADT", or "*"
	Response      string  `json:"response"`       // "AA", "AE", or "AR"
	ErrorCode     int     `json:"error_code"`     // HL7 error code, defaults to 207
	ErrorSeverity string  `json:"error_severity"` // "E", "W", or "F"
	ErrorMsg      string  `json:"error_msg"`
	DelayMs       int     `json:"delay_ms"`  // artificial latency
	NackRate      float64 `json:"nack_rate"` // 0.0–1.0 probability to override with AR
	AckText       string  `json:"ack_text"`  // text in MSA.3
}

// RulesConfig is the top-level structure of the rules JSON file.
type RulesConfig struct {
	Rules []Rule `json:"rules"`
}

func parseRules(data []byte) (*RulesConfig, error) {
	var cfg RulesConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// loadRules loads rules from path, falling back to the embedded rules.json.
func loadRules(path string) (*RulesConfig, string, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err == nil {
		cfg, err := parseRules(data)
		if err != nil {
			return nil, path, fmt.Errorf("parse rules file %q: %w", path, err)
		}
		return cfg, path, nil
	}
	cfg, err := parseRules(defaultRules)
	if err != nil {
		return nil, "embedded", fmt.Errorf("parse embedded rules: %w", err)
	}
	return cfg, "embedded", nil
}

// matchRule returns the best-matching rule for a given message type and trigger event.
// Priority: exact "MsgType^TriggerEvent" > message type only > wildcard "*".
func matchRule(rules []Rule, msgType, trigEvt string) *Rule {
	exact := strings.ToUpper(msgType + "^" + trigEvt)
	for i := range rules {
		if strings.ToUpper(rules[i].Match) == exact {
			return &rules[i]
		}
	}
	upper := strings.ToUpper(msgType)
	for i := range rules {
		if strings.ToUpper(rules[i].Match) == upper {
			return &rules[i]
		}
	}
	for i := range rules {
		if rules[i].Match == "*" {
			return &rules[i]
		}
	}
	return nil
}

// buildResponse constructs an HL7 ACK/NACK response for any acknowledgment code.
func buildResponse(msh []string, ackCode string, errCode int, errSeverity, errMsg, ackText string) []byte {
	ctrlID := field(msh, 9)
	procID := field(msh, 10)
	version := field(msh, 11)
	msgType := fmt.Sprintf("ACK^%s^ACK", triggerEvent(field(msh, 8)))

	var sb strings.Builder
	fmt.Fprintf(&sb, "MSH|^~\\&|mllpong|mllpong|%s|%s|%s||%s|%s|%s|%s\r",
		field(msh, 2), field(msh, 3), timestamp(), msgType, ctrlID, procID, version)
	fmt.Fprintf(&sb, "MSA|%s|%s|%s\r", ackCode, ctrlID, ackText)
	if ackCode != "AA" {
		if errCode <= 0 {
			errCode = 207
		}
		if errSeverity == "" {
			errSeverity = "E"
		}
		fmt.Fprintf(&sb, "ERR|||%d||%s||%s\r", errCode, errSeverity, errMsg)
	}
	return wrapMLLP([]byte(sb.String()))
}

// smartHandler returns a handlerFunc that dispatches responses based on loaded rules.
func smartHandler(cfg *RulesConfig) handlerFunc {
	return func(raw string) []byte {
		log.Println("Handling message with SmartHandler")
		msh, err := parseMSH(raw)
		if err != nil {
			log.Printf("Parse error: %v", err)
			return buildLastDitchError(err)
		}

		msh9 := field(msh, 8)
		msgType := messageType(msh9)
		trigEvt := triggerEvent(msh9)

		rule := matchRule(cfg.Rules, msgType, trigEvt)
		if rule == nil {
			log.Printf("No rule matched for %s^%s, defaulting to AA", msgType, trigEvt)
			return buildACK(msh)
		}

		log.Printf("Rule matched: match=%q response=%s delay_ms=%d nack_rate=%.2f",
			rule.Match, rule.Response, rule.DelayMs, rule.NackRate)

		if rule.DelayMs > 0 {
			time.Sleep(time.Duration(rule.DelayMs) * time.Millisecond)
		}

		ackCode := rule.Response
		if ackCode == "" {
			ackCode = "AA"
		}
		if rule.NackRate > 0 && rand.Float64() < rule.NackRate {
			ackCode = "AR"
			log.Printf("Nack rate triggered (rate=%.2f), overriding to AR", rule.NackRate)
		}

		return buildResponse(msh, ackCode, rule.ErrorCode, rule.ErrorSeverity, rule.ErrorMsg, rule.AckText)
	}
}

type handlerFunc func(raw string) []byte

func ackAllHandler(raw string) []byte {
	log.Println("Handling message with AckAllHandler")
	msh, err := parseMSH(raw)
	if err != nil {
		log.Printf("Parse error: %v", err)
		return buildLastDitchError(err)
	}
	resp := buildACK(msh)
	log.Printf("Replying with ACK to ctrl-id=%s", field(msh, 9))
	return resp
}

func chaosHandler(raw string) []byte {
	log.Println("Handling message with ChaosHandler")
	msh, err := parseMSH(raw)
	if err != nil {
		log.Printf("Parse error: %v", err)
		return buildLastDitchError(err)
	}
	return buildNACK(msh, 207, "E", "Unknown error occurred: Eeeevil!")
}

var saveSeq atomic.Uint64

// safeFilePart sanitizes a network-supplied value for use in a file name.
// Anything outside [A-Za-z0-9-] becomes "_", capped at 64 characters.
func safeFilePart(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
		if sb.Len() >= 64 {
			break
		}
	}
	if sb.Len() == 0 {
		return "unknown"
	}
	return sb.String()
}

// saveFileName builds the file name for an inbound message.
// Format: "<timestamp>-<sequence>-<message type>-<control ID>.hl7".
func saveFileName(msh []string) string {
	msh9 := field(msh, 8)
	label := messageType(msh9)
	if trigEvt := triggerEvent(msh9); trigEvt != "" {
		label += "-" + trigEvt
	}
	return fmt.Sprintf("%s-%06d-%s-%s.hl7",
		time.Now().Format("20060102-150405.000"),
		saveSeq.Add(1),
		safeFilePart(label),
		safeFilePart(field(msh, 9)))
}

// saveMessage writes an inbound message to dir and returns its path.
// Messages without a parsable MSH segment are saved as "unknown".
func saveMessage(dir, raw string) (string, error) {
	msh, _ := parseMSH(raw)
	path := filepath.Join(dir, saveFileName(msh))
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		return "", fmt.Errorf("write %q: %w", path, err)
	}
	return path, nil
}

// withSaver wraps handler to persist every inbound message to dir.
// An empty dir disables saving.
func withSaver(dir string, handler handlerFunc) handlerFunc {
	if dir == "" {
		return handler
	}
	return func(raw string) []byte {
		path, err := saveMessage(dir, raw)
		if err != nil {
			log.Printf("Save error: %v", err)
		} else {
			log.Printf("Saved inbound message to %s", path)
		}
		return handler(raw)
	}
}

func handleConn(conn net.Conn, handler handlerFunc) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		raw, err := readMLLP(reader)
		if err != nil {
			if err != io.EOF {
				log.Printf("Read error from %s: %v", conn.RemoteAddr(), err)
			}
			return
		}
		resp := handler(string(raw))
		if _, err := conn.Write(resp); err != nil {
			log.Printf("Write error to %s: %v", conn.RemoteAddr(), err)
			return
		}
	}
}

func serve(ln net.Listener, handler handlerFunc, wg *sync.WaitGroup) {
	defer wg.Done()
	defer ln.Close()
	log.Printf("Listening on %s", ln.Addr())
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("Accept error on %s: %v", ln.Addr(), err)
			return
		}
		go handleConn(conn, handler)
	}
}

func mustListen(addr string) net.Listener {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", addr, err)
	}
	return ln
}

func run(host, ackPort, chaosPort, smartPort, rulesFile, savePath string) {
	var wg sync.WaitGroup

	if savePath != "" {
		if err := os.MkdirAll(savePath, 0o755); err != nil {
			log.Fatalf("Failed to create save path %s: %v", savePath, err)
		}
		log.Printf("💾 SAVING INBOUND MESSAGES TO %s", savePath)
	}

	log.Printf("👹 STARTING CHAOS HANDLER ON PORT %s", chaosPort)
	wg.Add(1)
	go serve(mustListen(host+":"+chaosPort), withSaver(savePath, chaosHandler), &wg)

	log.Printf("👍 STARTING ALWAYS ACK SERVER ON PORT %s", ackPort)
	wg.Add(1)
	go serve(mustListen(host+":"+ackPort), withSaver(savePath, ackAllHandler), &wg)

	rules, rulesSource, err := loadRules(rulesFile)
	if err != nil {
		log.Fatalf("Failed to load rules: %v", err)
	}
	log.Printf("🧠 STARTING SMART HANDLER ON PORT %s (rules: %s, %d rules loaded)", smartPort, rulesSource, len(rules.Rules))
	wg.Add(1)
	go serve(mustListen(host+":"+smartPort), withSaver(savePath, smartHandler(rules)), &wg)

	wg.Wait()
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	host := flag.String("host", envOr("HOST", "0.0.0.0"), "interface to bind")
	ackPort := flag.String("ack-port", envOr("ACK_PORT", "2575"), "port for the always-ACK handler")
	chaosPort := flag.String("chaos-port", envOr("CHAOS_PORT", "2576"), "port for the always-NACK chaos handler")
	smartPort := flag.String("smart-port", envOr("SMART_PORT", "2577"), "port for the rule-based smart handler")
	rulesFile := flag.String("rules-file", envOr("RULES_FILE", "rules.json"), "path to the smart handler rules JSON file")
	savePath := flag.String("save-path", envOr("SAVE_PATH", ""), "directory to save every inbound message to (saving is off when empty)")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Println("STARTING MLLPONG 🏓")
	log.Println("---")

	run(*host, *ackPort, *chaosPort, *smartPort, *rulesFile, *savePath)
}
