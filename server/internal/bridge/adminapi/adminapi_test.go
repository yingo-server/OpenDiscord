package adminapi

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yingo-server/UFOT/server/internal/proto"
)

type fakeBus struct {
	topics []string
	frames []*proto.Frame
}

func (b *fakeBus) Pub(topic string, f *proto.Frame) {
	b.topics = append(b.topics, topic)
	b.frames = append(b.frames, f)
}

type denyAll struct{}

func (denyAll) Authorize(r *http.Request) error { return errors.New("no") }

func TestInjectPublishesToBus(t *testing.T) {
	fb := &fakeBus{}
	api := New(nil, fb)
	body := `{"kind":101,"conv_id":42,"payload_b64":"` + base64.StdEncoding.EncodeToString([]byte("hi")) + `"}`
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/inject", bytes.NewBufferString(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(fb.frames) != 1 {
		t.Fatalf("frames=%d", len(fb.frames))
	}
	f := fb.frames[0]
	if f.Kind != 101 || f.Env.GetConvId() != 42 || string(f.Env.GetPayload()) != "hi" {
		t.Fatalf("frame=%+v env=%+v", f, f.Env)
	}
	if fb.topics[0] != "conv.42" {
		t.Fatalf("topic=%s", fb.topics[0])
	}
	if api.Injected() != 1 {
		t.Fatal("injected counter")
	}
}

func TestInjectUserTopic(t *testing.T) {
	fb := &fakeBus{}
	api := New(nil, fb)
	body := `{"kind":500,"user_id":7}`
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/inject", bytes.NewBufferString(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code=%d", rec.Code)
	}
	if fb.topics[0] != "user.7" {
		t.Fatalf("topic=%s", fb.topics[0])
	}
}

func TestUnauthorized(t *testing.T) {
	fb := &fakeBus{}
	api := New(denyAll{}, fb)
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/inject", bytes.NewBufferString(`{"kind":101}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
	if len(fb.frames) != 0 {
		t.Fatal("must not inject when unauthorized")
	}
}

func TestBadRequests(t *testing.T) {
	fb := &fakeBus{}
	api := New(nil, fb)
	for _, body := range []string{
		`{not json`,
		`{"kind":0}`,
		`{"kind":101,"payload_b64":"!!!"}`,
	} {
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/inject", bytes.NewBufferString(body)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%q code=%d", body, rec.Code)
		}
	}
}
