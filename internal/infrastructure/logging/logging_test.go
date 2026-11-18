package logging

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

const secret = "s3cr3t-pa55"

func TestRedaction(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			r := NewRedactor()
			r.Add(secret)
			log, err := New(&buf, format, slog.LevelDebug, r)
			if err != nil {
				t.Fatal(err)
			}
			log.Info("connecting with "+secret,
				"password", secret,
				"dsn", "postgres://app:"+secret+"@db",
				"err", errors.New("auth failed for "+secret),
			)
			log.WithGroup("db").Info("nested", "password", secret)

			out := buf.String()
			if strings.Contains(out, secret) {
				t.Fatalf("secret leaked:\n%s", out)
			}
			if got := strings.Count(out, redacted); got != 5 {
				t.Fatalf("want 5 redactions, got %d:\n%s", got, out)
			}
		})
	}
}

func TestRedactOverlappingSecrets(t *testing.T) {
	r := NewRedactor()
	r.Add("abc")
	r.Add("abcdef")
	if got := r.Redact("x abcdef y"); got != "x "+redacted+" y" {
		t.Fatalf("got %q", got)
	}
}

func TestEmptySecretIgnored(t *testing.T) {
	r := NewRedactor()
	r.Add("")
	if got := r.Redact("hello"); got != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestLevel(t *testing.T) {
	var buf bytes.Buffer
	log, err := New(&buf, "text", slog.LevelWarn, NewRedactor())
	if err != nil {
		t.Fatal(err)
	}
	log.Debug("hidden")
	log.Info("hidden")
	log.Warn("shown")
	if out := buf.String(); strings.Contains(out, "hidden") || !strings.Contains(out, "shown") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestInvalidFormat(t *testing.T) {
	if _, err := New(&bytes.Buffer{}, "xml", slog.LevelInfo, NewRedactor()); err == nil {
		t.Fatal("expected error")
	}
}
