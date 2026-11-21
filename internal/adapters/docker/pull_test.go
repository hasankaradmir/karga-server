package docker

import (
	"errors"
	"iter"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/jsonstream"
)

func messages(ms ...jsonstream.Message) iter.Seq2[jsonstream.Message, error] {
	return func(yield func(jsonstream.Message, error) bool) {
		for _, m := range ms {
			if !yield(m, nil) {
				return
			}
		}
	}
}

func TestReadPullProgress(t *testing.T) {
	dl := func(id string, cur, total int64) jsonstream.Message {
		return jsonstream.Message{ID: id, Status: "Downloading", Progress: &jsonstream.Progress{Current: cur, Total: total}}
	}
	var got [][2]int64
	err := readPull(messages(
		jsonstream.Message{Status: "Pulling from library/postgres"},
		jsonstream.Message{ID: "a", Status: "Pulling fs layer"},
		dl("a", 10, 100),
		dl("b", 5, 50),
		jsonstream.Message{ID: "a", Status: "Download complete"},
		jsonstream.Message{ID: "a", Status: "Extracting", Progress: &jsonstream.Progress{Current: 90, Total: 100}}, // not downloading
		dl("b", 50, 50),
	), func(c, total int64) { got = append(got, [2]int64{c, total}) })
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]int64{{10, 100}, {15, 150}, {105, 150}, {150, 150}}
	if len(got) != len(want) {
		t.Fatalf("progress %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("progress %v, want %v", got, want)
		}
	}
	// Without a callback the messages are only read.
	if err := readPull(messages(dl("a", 1, 2)), nil); err != nil {
		t.Fatal(err)
	}
}

func TestReadPullError(t *testing.T) {
	err := readPull(messages(jsonstream.Message{Error: &jsonstream.Error{Code: 404, Message: "manifest unknown"}}), nil)
	if err == nil || err.Error() != "manifest unknown" || !cerrdefs.IsNotFound(err) {
		t.Fatalf("got %v (not found %v)", err, cerrdefs.IsNotFound(err))
	}
	boom := errors.New("connection reset")
	stream := func(yield func(jsonstream.Message, error) bool) { yield(jsonstream.Message{}, boom) }
	if err := readPull(stream, nil); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}
