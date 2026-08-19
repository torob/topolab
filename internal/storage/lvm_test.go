package storage

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type call struct {
	name string
	args []string
}
type fakeRunner struct {
	calls   []call
	outputs [][]byte
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, call{name, append([]string(nil), args...)})
	if len(f.outputs) == 0 {
		return nil, nil
	}
	out := f.outputs[0]
	f.outputs = f.outputs[1:]
	return out, nil
}

func TestMachineReadableReports(t *testing.T) {
	r := &fakeRunner{outputs: [][]byte{[]byte(`{"report":[{"vg":[{"vg_name":"vg0","vg_uuid":"vg-id","vg_size":"10737418240","vg_free":"5368709120","pv_count":"1"}]}]}`), []byte(`{"report":[{"lv":[{"lv_name":"pool","lv_uuid":"lv-id","vg_name":"vg0","vg_uuid":"vg-id","lv_size":"4294967296","pool_lv":"","origin":"","lv_attr":"twi-a-tz--","lv_tags":"topolab,topolab.kind.pool","data_percent":"80.25","metadata_percent":"2.5"}]}]}`)}}
	lvm := NewLVM(r, "/wrapper/lvm")
	vg, err := lvm.LookupVG(context.Background(), "vg0")
	if err != nil || vg.UUID != "vg-id" || vg.FreeBytes != 5368709120 {
		t.Fatalf("VG = %#v, %v", vg, err)
	}
	lvs, err := lvm.LVs(context.Background(), "vg0")
	if err != nil || len(lvs) != 1 || lvs[0].DataPercent != 80.25 || !HasTags(lvs[0].Tags, []string{"topolab.kind.pool"}) {
		t.Fatalf("LVs = %#v, %v", lvs, err)
	}
	for _, c := range r.calls {
		if c.name != "/wrapper/lvm" || !containsPair(c.args, "--reportformat", "json") {
			t.Fatalf("not a machine-readable overridden call: %#v", c)
		}
	}
}

func TestReportsRejectMissingAndAmbiguousIdentities(t *testing.T) {
	tests := []struct {
		name string
		out  string
		read func(*LVM) error
	}{
		{
			name: "VG missing UUID",
			out:  `{"report":[{"vg":[{"vg_name":"vg0","vg_size":"1","vg_free":"1","pv_count":"1"}]}]}`,
			read: func(l *LVM) error { _, err := l.VGs(context.Background()); return err },
		},
		{
			name: "duplicate LV name",
			out: `{"report":[{"lv":[
				{"lv_name":"disk","lv_uuid":"one","vg_name":"vg0","vg_uuid":"vg-id","lv_size":"1","lv_attr":"-wi-a-----"},
				{"lv_name":"disk","lv_uuid":"two","vg_name":"vg0","vg_uuid":"vg-id","lv_size":"1","lv_attr":"-wi-a-----"}
			]}]}`,
			read: func(l *LVM) error { _, err := l.LVs(context.Background(), "vg0"); return err },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.read(NewLVM(&fakeRunner{outputs: [][]byte{[]byte(test.out)}}, "/wrapper/lvm"))
			if err == nil {
				t.Fatal("unsafe report was accepted")
			}
		})
	}
}

type failingRunner struct{}

func (failingRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return nil, fmt.Errorf("injected failure")
}

func TestMutationErrorIncludesCommandAndTarget(t *testing.T) {
	err := NewLVM(failingRunner{}, "/wrapper/lvm").CreateThick(context.Background(), "vg0", "disk0", 1, false, nil)
	if err == nil || !strings.Contains(err.Error(), "lvcreate") || !strings.Contains(err.Error(), "vg0") || !strings.Contains(err.Error(), "disk0") {
		t.Fatalf("error lacks command target context: %v", err)
	}
}

func TestLVMCreationPlansAreTargeted(t *testing.T) {
	r := &fakeRunner{}
	lvm := NewLVM(r, "lvm-test")
	ctx := context.Background()
	tags := []string{"topolab", "topolab.kind.disk"}
	if err := lvm.CreatePool(ctx, "vg0", "pool0", 4<<30, false, tags); err != nil {
		t.Fatal(err)
	}
	if err := lvm.CreatePool(ctx, "vg0", "poolz", 4<<30, true, tags); err != nil {
		t.Fatal(err)
	}
	if err := lvm.CreateThin(ctx, "vg0", "pool0", "disk0", 2<<30, tags); err != nil {
		t.Fatal(err)
	}
	if err := lvm.CreateThick(ctx, "vg0", "thick0", 2<<30, false, tags); err != nil {
		t.Fatal(err)
	}
	if err := lvm.CreateThick(ctx, "vg0", "thickz", 2<<30, true, tags); err != nil {
		t.Fatal(err)
	}
	if err := lvm.Snapshot(ctx, "vg0", "base0", "root0", tags); err != nil {
		t.Fatal(err)
	}
	if err := lvm.Activate(ctx, "vg0", "root0"); err != nil {
		t.Fatal(err)
	}
	if err := lvm.Extend(ctx, "vg0", "disk0", 3<<30); err != nil {
		t.Fatal(err)
	}
	if err := lvm.Remove(ctx, "vg0", "disk0"); err != nil {
		t.Fatal(err)
	}
	joined := make([]string, len(r.calls))
	for i, c := range r.calls {
		joined[i] = fmt.Sprintf("%s %s", c.name, strings.Join(c.args, " "))
	}
	wants := []string{"lvcreate --yes --type thin-pool --zero n --size 4294967296B --name pool0", "lvcreate --yes --type thin-pool --zero y --size 4294967296B --name poolz", "--thinpool vg0/pool0 --name disk0", "--wipesignatures y --zero n", "--wipesignatures y --zero y", "--snapshot --name root0", "lvchange --setactivationskip n vg0/root0", "lvchange --activate y vg0/root0", "lvextend --yes --size 3221225472B vg0/disk0", "lvremove --yes vg0/disk0"}
	for _, want := range wants {
		found := false
		for _, got := range joined {
			if strings.Contains(got, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %q in %#v", want, joined)
		}
	}
	for _, forbidden := range []string{"100%FREE", "--select", "--all"} {
		if strings.Contains(strings.Join(joined, "\n"), forbidden) {
			t.Errorf("unsafe broad argument %q", forbidden)
		}
	}
	for _, command := range joined {
		if strings.Contains(command, "--thinpool vg0/pool0") && strings.Contains(command, "--zero") {
			t.Errorf("individual thin LV incorrectly requested --zero: %s", command)
		}
	}
}

func TestDeterministicNamesAndTags(t *testing.T) {
	if PoolName("lab", "thin") != PoolName("lab", "thin") || DiskName("lab", "vm", "root") == DiskName("lab", "vm2", "root") {
		t.Fatal("names are not deterministic and scoped")
	}
	if got := Tags("lab", "thin", "pool"); len(got) != 4 || got[3] != "topolab.kind.pool" {
		t.Fatalf("tags = %v", got)
	}
	if !reflect.DeepEqual(Tags("lab", "thin", "pool"), Tags("lab", "thin", "pool")) {
		t.Fatal("tags changed")
	}
}

func containsPair(values []string, a, b string) bool {
	for i := 0; i+1 < len(values); i++ {
		if values[i] == a && values[i+1] == b {
			return true
		}
	}
	return false
}
