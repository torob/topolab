package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/torob/topolab/internal/remote/blob"
	"github.com/torob/topolab/internal/remote/jobs"
	"github.com/torob/topolab/internal/remote/wire"
)

// adminCommands manage the job and blob stores. They run in the session.
var adminCommands = map[string]bool{
	"jobs-ls": true, "jobs-show": true, "jobs-attach": true, "jobs-cancel": true, "jobs-prune": true,
	"templates-ls": true, "templates-prune": true,
}

func (s *session) runAdmin(req wire.Request) (int, error) {
	p, err := parseRequest(req)
	if err != nil {
		return s.reject(err)
	}
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	s.setCancel(cancel)
	out := streamWriter{conn: s.conn, typ: wire.TypeStdout}
	store := s.jobStore(p.workingDir)
	topology := p.cfg.Name
	arg := func() string {
		if len(req.Args) > 0 {
			return req.Args[0]
		}
		return ""
	}
	switch req.Command {
	case "jobs-ls":
		return s.finishAdmin(s.jobsList(ctx, store, topology, len(req.Args) == 1 && req.Args[0] == "--all", out))
	case "jobs-show":
		return s.finishAdmin(jobsShow(store, topology, arg(), out))
	case "jobs-attach":
		return s.streamJob(s.ctx, store, topology, arg(), req.Offset)
	case "jobs-cancel":
		return s.finishAdmin(s.jobsCancel(ctx, store, topology, arg(), out))
	case "jobs-prune":
		removed, err := store.Prune(topology)
		if err == nil {
			_, err = fmt.Fprintf(out, "removed %d finished job(s)\n", len(removed))
		}
		return s.finishAdmin(err)
	case "templates-ls":
		return s.finishAdmin(templatesList(store, p, out))
	case "templates-prune":
		return s.finishAdmin(templatesPrune(ctx, store, p, req.Args, s.opts.Now(), out))
	}
	return s.reject(reqErr(wire.CodeProtocolError, "unknown command %q", req.Command))
}

// finishAdmin maps an admin command's result to an exit status.
func (s *session) finishAdmin(err error) (int, error) {
	if err == nil {
		return 0, s.streamError()
	}
	var re *requestError
	if errors.As(err, &re) {
		return s.reject(err)
	}
	if errors.Is(err, context.Canceled) {
		return 1, nil
	}
	return 1, s.writeStderr(fmt.Sprintf("topolab: %v\n", err))
}

func (s *session) writeStderr(text string) error {
	_, err := streamWriter{conn: s.conn, typ: wire.TypeStderr}.Write([]byte(text))
	return err
}

func timeOrDash(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func (s *session) jobsList(ctx context.Context, store *jobs.Store, topology string, all bool, out streamWriter) error {
	scope := topology
	if all {
		scope = ""
	}
	list, err := store.List(scope)
	if err != nil {
		return err
	}
	for i := range list {
		if jobs.Terminal(list[i].State) || store.Alive(&list[i]) {
			continue
		}
		updated := s.markLost(ctx, store, &list[i])
		list[i] = *updated
	}
	w := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tTOPOLOGY\tCOMMAND\tSTATE\tSTARTED\tFINISHED\tEXIT")
	for _, st := range list {
		exit := "-"
		if jobs.Terminal(st.State) {
			exit = fmt.Sprint(st.ExitCode)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", st.ID, st.Topology, st.Command, st.State, timeOrDash(st.Started), timeOrDash(st.Finished), exit)
	}
	return w.Flush()
}

func jobsShow(store *jobs.Store, topology, id string, out streamWriter) error {
	st, err := store.Load(topology, id)
	if err != nil {
		return jobLookupError(err, id)
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	_, err = out.Write(append(raw, '\n'))
	return err
}

func (s *session) jobsCancel(ctx context.Context, store *jobs.Store, topology, id string, out streamWriter) error {
	if _, err := store.Load(topology, id); err != nil {
		return jobLookupError(err, id)
	}
	s.signalRunner(store, topology, id)
	for {
		st, err := store.Load(topology, id)
		if err != nil {
			return err
		}
		if !jobs.Terminal(st.State) && !store.Alive(st) {
			st = s.markLost(ctx, store, st)
		}
		if jobs.Terminal(st.State) {
			_, err := fmt.Fprintf(out, "job %s is %s\n", id, st.State)
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.opts.PollInterval):
		}
	}
}

func templatesList(jobStore *jobs.Store, p *parsed, out streamWriter) error {
	store := blob.NewStore(p.workingDir)
	list, err := store.List()
	if err != nil {
		return err
	}
	refs, err := store.References(p.workingDir)
	if err != nil {
		return err
	}
	active, err := jobStore.ActiveTemplates()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	fmt.Fprintln(w, "DIGEST\tSIZE\tLAST-USED\tREFERENCED-BY")
	for _, b := range list {
		var names []string
		for name := range refs[b.Digest] {
			names = append(names, name)
		}
		sort.Strings(names)
		if active[b.Digest] {
			names = append(names, "(running job)")
		}
		by := "-"
		if len(names) > 0 {
			by = strings.Join(names, ",")
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", b.Digest, b.Size, b.LastUsed.UTC().Format(time.RFC3339), by)
	}
	return w.Flush()
}

func templatesPrune(ctx context.Context, jobStore *jobs.Store, p *parsed, args []string, now time.Time, out streamWriter) error {
	minAge := 24 * time.Hour
	if len(args) == 2 && args[0] == "--min-age" {
		d, err := time.ParseDuration(args[1])
		if err != nil || d < 0 {
			return reqErr(wire.CodeProtocolError, "invalid --min-age %q", args[1])
		}
		minAge = d
	} else if len(args) != 0 {
		return reqErr(wire.CodeProtocolError, "templates prune accepts only --min-age D")
	}
	protected, err := jobStore.ActiveTemplates()
	if err != nil {
		return err
	}
	res, err := blob.NewStore(p.workingDir).Prune(ctx, p.workingDir, blob.PruneOptions{MinAge: minAge, Now: now, Protected: protected})
	for _, d := range res.Removed {
		fmt.Fprintf(out, "removed %s\n", d)
	}
	for _, d := range res.RemovedPartial {
		fmt.Fprintf(out, "removed partial %s\n", d)
	}
	if err != nil {
		return err
	}
	if len(res.Removed)+len(res.RemovedPartial) == 0 {
		_, err = fmt.Fprintln(out, "nothing to remove")
	}
	return err
}
