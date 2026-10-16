package checkout

// Readable logs for every step of a purchase.
//
// LOG_FORMAT=pretty (default with test keys) → one coloured line per step:
//   18:21:04.120  CHECKOUT   ② Priced the order   org=01JA2Y6H… qty=500 total=USD 500.00
// LOG_FORMAT=json (default with live keys)    → one JSON object per line for CloudWatch / Datadog / Loki.
//
// PRODUCTION: keep LOG_FORMAT=json, ship stdout to your log platform, and alert on
// level=error with cat=FULFIL (paid-but-not-credited) and cat=WEBHOOK (signature failures).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Level int

const (
	LevelDebug Level = 10
	LevelInfo  Level = 20
	LevelWarn  Level = 30
	LevelError Level = 40
)

var levelNames = map[Level]string{LevelDebug: "debug", LevelInfo: "info", LevelWarn: "warn", LevelError: "error"}
var levelMarks = map[Level]string{LevelDebug: " ", LevelInfo: " ", LevelWarn: "⚠", LevelError: "✖"}
var catColors = map[string]int{"HTTP": 90, "AUTH": 35, "CHECKOUT": 36, "STRIPE": 34, "WEBHOOK": 33, "FULFIL": 32, "LEDGER": 92, "STATUS": 37, "SYSTEM": 97}
var secretKey = regexp.MustCompile(`(?i)secret|password|token|authorization|cookie|card`)

// Logger writes pretty or JSON lines. Safe for concurrent use.
type Logger struct {
	mu   sync.Mutex
	out  io.Writer
	json bool
	min  Level
}

func NewLogger(out io.Writer, format, level string) *Logger {
	min := LevelInfo
	for l, n := range levelNames {
		if n == level {
			min = l
		}
	}
	return &Logger{out: out, json: format == "json", min: min}
}

func (l *Logger) Debug(cat, msg string, kv ...any) { l.log(LevelDebug, cat, msg, kv) }
func (l *Logger) Info(cat, msg string, kv ...any)  { l.log(LevelInfo, cat, msg, kv) }
func (l *Logger) Warn(cat, msg string, kv ...any)  { l.log(LevelWarn, cat, msg, kv) }
func (l *Logger) Error(cat, msg string, kv ...any) { l.log(LevelError, cat, msg, kv) }

// log takes key/value pairs in order. Empty values are dropped; secret-looking keys are redacted.
func (l *Logger) log(level Level, cat, msg string, kv []any) {
	if l == nil || level < l.min {
		return
	}
	type pair struct {
		k string
		v any
	}
	var pairs []pair
	for i := 0; i+1 < len(kv); i += 2 {
		k := fmt.Sprint(kv[i])
		v := kv[i+1]
		if v == nil || v == "" {
			continue
		}
		if secretKey.MatchString(k) {
			v = "[redacted]"
		}
		pairs = append(pairs, pair{k, v})
	}

	var buf bytes.Buffer
	now := time.Now()
	if l.json {
		buf.WriteString(`{"time":`)
		writeJSON(&buf, now.UTC().Format(time.RFC3339Nano))
		buf.WriteString(`,"level":`)
		writeJSON(&buf, levelNames[level])
		buf.WriteString(`,"cat":`)
		writeJSON(&buf, cat)
		buf.WriteString(`,"msg":`)
		writeJSON(&buf, msg)
		for _, p := range pairs {
			buf.WriteByte(',')
			writeJSON(&buf, p.k)
			buf.WriteByte(':')
			if e, ok := p.v.(error); ok {
				writeJSON(&buf, e.Error())
			} else {
				writeJSON(&buf, p.v)
			}
		}
		buf.WriteString("}\n")
	} else {
		color := catColors[cat]
		if color == 0 {
			color = 37
		}
		lvl := ""
		switch level {
		case LevelError:
			lvl = "\x1b[31m"
		case LevelWarn:
			lvl = "\x1b[33m"
		}
		fmt.Fprintf(&buf, "\x1b[2m%s\x1b[0m  \x1b[%dm%-8s\x1b[0m %s%s %s\x1b[0m", now.Format("15:04:05.000"), color, cat, lvl, levelMarks[level], msg)
		if len(pairs) > 0 {
			buf.WriteString("  ")
			for _, p := range pairs {
				fmt.Fprintf(&buf, " \x1b[2m%s=\x1b[0m%v", p.k, p.v)
			}
		}
		buf.WriteByte('\n')
	}
	l.mu.Lock()
	_, _ = l.out.Write(buf.Bytes())
	l.mu.Unlock()
}

func writeJSON(buf *bytes.Buffer, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(fmt.Sprint(v))
	}
	buf.Write(b)
}

// zeroDecimal lists currencies Stripe charges in whole units (no cents).
var zeroDecimal = map[string]bool{"bif": true, "clp": true, "djf": true, "gnf": true, "jpy": true, "kmf": true, "krw": true,
	"mga": true, "pyg": true, "rwf": true, "ugx": true, "vnd": true, "vuv": true, "xaf": true, "xof": true, "xpf": true}

// Money formats minor units for logs: 50000 usd → "USD 500.00".
func Money(minor int64, currency string) string {
	cur := strings.ToUpper(currency)
	if zeroDecimal[strings.ToLower(currency)] {
		return fmt.Sprintf("%s %s", cur, groupThousands(minor))
	}
	return fmt.Sprintf("%s %s.%02d", cur, groupThousands(minor/100), abs(minor%100))
}

func groupThousands(n int64) string {
	s := fmt.Sprint(abs(n))
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if n < 0 {
		return "-" + string(out)
	}
	return string(out)
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// Short keeps Stripe ids readable in logs: cs_test_a1B2c3d4e5f6… → cs_test_…e5f6
func Short(id string) string {
	if len(id) > 18 {
		return id[:8] + "…" + id[len(id)-6:]
	}
	return id
}
