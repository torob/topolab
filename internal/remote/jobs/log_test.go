package jobs

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func readAll(t *testing.T, path string) []Record {
	t.Helper()
	var out []Record
	var offset int64
	for {
		rec, ok, err := ReadRecord(path, offset)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return out
		}
		out = append(out, rec)
		offset = rec.End
	}
}

func TestLogRecordsRoundTripWithBoundaryOffsets(t *testing.T) {
	s, _, _ := newTestStore(t)
	st := newJob(t, s, "lab", 1, StateRunning)
	w, err := s.OpenLog("lab", st.ID)
	if err != nil {
		t.Fatal(err)
	}
	fmtOut := w.StreamWriter(StreamStdout)
	fmtErr := w.StreamWriter(StreamStderr)
	io.WriteString(fmtOut, "one\n")
	io.WriteString(fmtErr, "two\n")
	if err := w.WriteExit(7); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	recs := readAll(t, s.LogPath("lab", st.ID))
	if len(recs) != 3 {
		t.Fatalf("records = %+v", recs)
	}
	if recs[0].Stream != StreamStdout || string(recs[0].Payload) != "one\n" || recs[1].Stream != StreamStderr || recs[2].Stream != StreamExit || string(recs[2].Payload) != "7" {
		t.Fatalf("records = %+v", recs)
	}
	if recs[0].End != RecordSize(4) || recs[1].End != RecordSize(4)*2 {
		t.Fatalf("offsets = %d %d", recs[0].End, recs[1].End)
	}
	// Reading from a boundary offset resumes without repeating anything.
	rest, ok, err := ReadRecord(s.LogPath("lab", st.ID), recs[0].End)
	if err != nil || !ok || string(rest.Payload) != "two\n" {
		t.Fatalf("resumed record = %+v %v %v", rest, ok, err)
	}
}

func TestLargeWritesAreSplitAtSixtyFourKiB(t *testing.T) {
	s, _, _ := newTestStore(t)
	st := newJob(t, s, "lab", 1, StateRunning)
	w, _ := s.OpenLog("lab", st.ID)
	big := bytes.Repeat([]byte("x"), MaxRecordPayload*2+10)
	if n, err := w.Write(StreamStdout, big); err != nil || n != len(big) {
		t.Fatalf("write = %d %v", n, err)
	}
	_ = w.Close()
	recs := readAll(t, s.LogPath("lab", st.ID))
	if len(recs) != 3 || len(recs[0].Payload) != MaxRecordPayload || len(recs[2].Payload) != 10 {
		t.Fatalf("record sizes: %d records", len(recs))
	}
}

func TestIncompleteTrailingRecordMeansNotYet(t *testing.T) {
	s, _, _ := newTestStore(t)
	st := newJob(t, s, "lab", 1, StateRunning)
	w, _ := s.OpenLog("lab", st.ID)
	io.WriteString(w.StreamWriter(StreamStdout), "whole")
	_ = w.Close()
	path := s.LogPath("lab", st.ID)
	whole, _ := os.ReadFile(path)
	for _, cut := range []int{1, 3, 4, 5, 8} {
		if err := os.WriteFile(path, append(append([]byte(nil), whole...), whole[:cut]...), 0o600); err != nil {
			t.Fatal(err)
		}
		recs := readAll(t, path)
		if len(recs) != 1 || string(recs[0].Payload) != "whole" {
			t.Fatalf("cut %d: records = %+v", cut, recs)
		}
	}
}

func TestLogLimitRecordsOneMarkerAndStopsOutputButNotExit(t *testing.T) {
	s, _, _ := newTestStore(t)
	st := newJob(t, s, "lab", 1, StateRunning)
	w, _ := s.OpenLog("lab", st.ID)
	w.SetLimit(200)
	out := w.StreamWriter(StreamStdout)
	for i := 0; i < 50; i++ {
		if _, err := io.WriteString(out, strings.Repeat("y", 30)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.WriteExit(0); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	recs := readAll(t, s.LogPath("lab", st.ID))
	markers, exits := 0, 0
	for _, r := range recs {
		if strings.Contains(string(r.Payload), "log limit reached") {
			markers++
		}
		if r.Stream == StreamExit {
			exits++
		}
	}
	if markers != 1 || exits != 1 {
		t.Fatalf("markers=%d exits=%d records=%d", markers, exits, len(recs))
	}
	if recs[len(recs)-1].Stream != StreamExit {
		t.Fatal("the exit record is not last")
	}
}

func TestReadRecordRejectsCorruptLengths(t *testing.T) {
	path := t.TempDir() + "/log"
	if err := os.WriteFile(path, []byte{0xff, 0xff, 0xff, 0xff, 1}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadRecord(path, 0); err == nil {
		t.Fatal("corrupt record accepted")
	}
}
