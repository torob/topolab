// Package storage implements explicit file and LVM disk backends. It keeps
// LVM command construction and machine-readable inspection independent from
// the application lifecycle so ordinary tests can use an injected runner.
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/torob/topolab/internal/qemu"
	"github.com/torob/topolab/internal/state"
)

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type LVM struct {
	Runner     Runner
	Executable string
}

var validLVMName = regexp.MustCompile(`^[A-Za-z0-9_+][A-Za-z0-9_.+-]{0,126}$`)

func ValidName(name string) bool { return validLVMName.MatchString(name) }

func NewLVM(r Runner, executable string) *LVM {
	if r == nil {
		r = qemu.ExecRunner{}
	}
	if executable == "" {
		executable = "lvm"
	}
	return &LVM{Runner: r, Executable: executable}
}

type VG struct {
	Name, UUID           string
	SizeBytes, FreeBytes uint64
	PVCount              int
}
type LV struct {
	Name, UUID, VGName, VGUUID, Pool, Origin string
	SizeBytes                                uint64
	DataPercent, MetadataPercent             float64
	Attr                                     string
	ZeroNewBlocks                            bool
	Tags                                     []string
}

type reportEnvelope struct {
	Report []struct {
		VG []map[string]string `json:"vg"`
		LV []map[string]string `json:"lv"`
	} `json:"report"`
}

func (l *LVM) run(ctx context.Context, args ...string) ([]byte, error) {
	out, err := l.Runner.Run(ctx, l.Executable, args...)
	if err != nil {
		return out, fmt.Errorf("run %s %q: %w", l.Executable, args, err)
	}
	return out, nil
}

func requiredReportField(row map[string]string, key, resource string) (string, error) {
	value, present := row[key]
	value = strings.TrimSpace(value)
	if !present || value == "" {
		return "", fmt.Errorf("LVM %s report has empty or missing %s", resource, key)
	}
	return value, nil
}

func cleanNumber(s string) string { return strings.TrimSpace(strings.TrimSuffix(s, "B")) }
func uintField(v map[string]string, key string) (uint64, error) {
	s := cleanNumber(v[key])
	if s == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 || f > math.MaxUint64 {
		return 0, fmt.Errorf("invalid LVM %s %q", key, s)
	}
	return uint64(f), nil
}
func floatField(v map[string]string, key string) (float64, error) {
	s := strings.TrimSpace(v[key])
	if s == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid LVM %s %q", key, s)
	}
	return f, nil
}

func (l *LVM) VGs(ctx context.Context) ([]VG, error) {
	out, err := l.run(ctx, "vgs", "--reportformat", "json", "--units", "b", "--nosuffix", "-o", "vg_name,vg_uuid,vg_size,vg_free,pv_count")
	if err != nil {
		return nil, err
	}
	var r reportEnvelope
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("decode vgs report: %w", err)
	}
	if len(r.Report) == 0 {
		return nil, errors.New("decode vgs report: report section is missing")
	}
	var result []VG
	names, uuids := map[string]bool{}, map[string]bool{}
	for _, report := range r.Report {
		if report.VG == nil {
			return nil, errors.New("decode vgs report: vg section is missing")
		}
		for _, row := range report.VG {
			name, e := requiredReportField(row, "vg_name", "VG")
			if e != nil {
				return nil, e
			}
			uuid, e := requiredReportField(row, "vg_uuid", "VG")
			if e != nil {
				return nil, e
			}
			if names[name] || uuids[uuid] {
				return nil, fmt.Errorf("LVM VG report contains duplicate identity name=%q uuid=%q", name, uuid)
			}
			names[name], uuids[uuid] = true, true
			for _, field := range []string{"vg_size", "vg_free", "pv_count"} {
				if _, e = requiredReportField(row, field, "VG"); e != nil {
					return nil, e
				}
			}
			size, e := uintField(row, "vg_size")
			if e != nil {
				return nil, e
			}
			free, e := uintField(row, "vg_free")
			if e != nil {
				return nil, e
			}
			pv, e := strconv.Atoi(strings.TrimSpace(row["pv_count"]))
			if e != nil && row["pv_count"] != "" {
				return nil, e
			}
			result = append(result, VG{Name: name, UUID: uuid, SizeBytes: size, FreeBytes: free, PVCount: pv})
		}
	}
	return result, nil
}

func (l *LVM) LookupVG(ctx context.Context, name string) (VG, error) {
	vgs, err := l.VGs(ctx)
	if err != nil {
		return VG{}, err
	}
	for _, vg := range vgs {
		if vg.Name == name {
			return vg, nil
		}
	}
	return VG{}, fmt.Errorf("volume group %q does not exist", name)
}

func (l *LVM) LVs(ctx context.Context, vg string) ([]LV, error) {
	out, err := l.run(ctx, "lvs", "--reportformat", "json", "--units", "b", "--nosuffix", "-a", "-o", "lv_name,lv_uuid,vg_name,vg_uuid,lv_size,pool_lv,origin,lv_attr,lv_tags,data_percent,metadata_percent", vg)
	if err != nil {
		return nil, err
	}
	var r reportEnvelope
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("decode lvs report: %w", err)
	}
	if len(r.Report) == 0 {
		return nil, errors.New("decode lvs report: report section is missing")
	}
	var result []LV
	names, uuids := map[string]bool{}, map[string]bool{}
	for _, report := range r.Report {
		if report.LV == nil {
			return nil, errors.New("decode lvs report: lv section is missing")
		}
		for _, row := range report.LV {
			name, e := requiredReportField(row, "lv_name", "LV")
			if e != nil {
				return nil, e
			}
			uuid, e := requiredReportField(row, "lv_uuid", "LV")
			if e != nil {
				return nil, e
			}
			vgName, e := requiredReportField(row, "vg_name", "LV")
			if e != nil {
				return nil, e
			}
			vgUUID, e := requiredReportField(row, "vg_uuid", "LV")
			if e != nil {
				return nil, e
			}
			if names[name] || uuids[uuid] {
				return nil, fmt.Errorf("LVM LV report contains duplicate identity name=%q uuid=%q", name, uuid)
			}
			names[name], uuids[uuid] = true, true
			for _, field := range []string{"lv_size", "lv_attr"} {
				if _, e = requiredReportField(row, field, "LV"); e != nil {
					return nil, e
				}
			}
			size, e := uintField(row, "lv_size")
			if e != nil {
				return nil, e
			}
			data, e := floatField(row, "data_percent")
			if e != nil {
				return nil, e
			}
			meta, e := floatField(row, "metadata_percent")
			if e != nil {
				return nil, e
			}
			var tags []string
			for _, tag := range strings.Split(row["lv_tags"], ",") {
				if tag = strings.TrimSpace(tag); tag != "" {
					tags = append(tags, tag)
				}
			}
			sort.Strings(tags)
			attr := strings.TrimSpace(row["lv_attr"])
			result = append(result, LV{Name: name, UUID: uuid, VGName: vgName, VGUUID: vgUUID, SizeBytes: size, Pool: strings.TrimSpace(row["pool_lv"]), Origin: strings.TrimSpace(row["origin"]), Attr: attr, ZeroNewBlocks: len(attr) > 7 && attr[7] == 'z', Tags: tags, DataPercent: data, MetadataPercent: meta})
		}
	}
	return result, nil
}

func FindLV(lvs []LV, name string) (LV, bool) {
	for _, lv := range lvs {
		if lv.Name == name {
			return lv, true
		}
	}
	return LV{}, false
}
func HasTags(actual, expected []string) bool {
	set := map[string]bool{}
	for _, v := range actual {
		set[v] = true
	}
	for _, v := range expected {
		if !set[v] {
			return false
		}
	}
	return true
}

func PoolName(topology, storage string) string {
	return "tlp-" + state.ShortID(topology+"\x00"+storage, 20)
}
func DiskName(topology, node, disk string) string {
	return "tld-" + state.ShortID(topology+"\x00"+node+"\x00"+disk, 24)
}
func BaseName(topology, storage, digest string) string {
	return "tlb-" + state.ShortID(topology+"\x00"+storage+"\x00"+digest, 24)
}
func StagingName(topology, node, disk string) string {
	return "tls-" + state.ShortID(topology+"\x00"+node+"\x00"+disk, 24)
}
func Tags(topology, storageName, kind string) []string {
	return []string{"topolab", "topolab.topology." + state.ShortID(topology, 16), "topolab.storage." + state.ShortID(storageName, 16), "topolab.kind." + kind}
}

func tagArgs(tags []string) []string {
	var out []string
	for _, tag := range tags {
		out = append(out, "--addtag", tag)
	}
	return out
}
func bytesArg(n uint64) string { return strconv.FormatUint(n, 10) + "B" }

func (l *LVM) CreatePool(ctx context.Context, vg, name string, bytes uint64, zeroNewBlocks bool, tags []string) error {
	if bytes == 0 {
		return errors.New("thin pool size must be positive")
	}
	zero := "n"
	if zeroNewBlocks {
		zero = "y"
	}
	args := []string{"lvcreate", "--yes", "--type", "thin-pool", "--zero", zero, "--size", bytesArg(bytes), "--name", name}
	args = append(args, tagArgs(tags)...)
	args = append(args, vg)
	_, err := l.run(ctx, args...)
	return err
}
func (l *LVM) CreateThin(ctx context.Context, vg, pool, name string, bytes uint64, tags []string) error {
	// Thin-volume zeroing is a thin-pool property selected by CreatePool.
	// LVM rejects --zero for an individual thin-volume lvcreate.
	args := []string{"lvcreate", "--yes", "--virtualsize", bytesArg(bytes), "--thinpool", vg + "/" + pool, "--name", name}
	args = append(args, tagArgs(tags)...)
	_, err := l.run(ctx, args...)
	return err
}
func (l *LVM) CreateThick(ctx context.Context, vg, name string, bytes uint64, zero bool, tags []string) error {
	zeroMode := "n"
	if zero {
		zeroMode = "y"
	}
	args := []string{"lvcreate", "--yes", "--wipesignatures", "y", "--zero", zeroMode, "--size", bytesArg(bytes), "--name", name}
	args = append(args, tagArgs(tags)...)
	args = append(args, vg)
	_, err := l.run(ctx, args...)
	return err
}
func (l *LVM) Snapshot(ctx context.Context, vg, origin, name string, tags []string) error {
	args := []string{"lvcreate", "--yes", "--snapshot", "--name", name}
	args = append(args, tagArgs(tags)...)
	args = append(args, vg+"/"+origin)
	_, err := l.run(ctx, args...)
	return err
}
func (l *LVM) Rename(ctx context.Context, vg, old, new string) error {
	_, err := l.run(ctx, "lvrename", vg, old, new)
	return err
}
func (l *LVM) SetReadOnly(ctx context.Context, vg, name string) error {
	_, err := l.run(ctx, "lvchange", "--permission", "r", vg+"/"+name)
	return err
}
func (l *LVM) Activate(ctx context.Context, vg, name string) error {
	if _, err := l.run(ctx, "lvchange", "--setactivationskip", "n", vg+"/"+name); err != nil {
		return err
	}
	_, err := l.run(ctx, "lvchange", "--activate", "y", vg+"/"+name)
	return err
}
func (l *LVM) DeleteTag(ctx context.Context, vg, name, tag string) error {
	_, err := l.run(ctx, "lvchange", "--deltag", tag, vg+"/"+name)
	return err
}
func (l *LVM) Extend(ctx context.Context, vg, name string, bytes uint64) error {
	_, err := l.run(ctx, "lvextend", "--yes", "--size", bytesArg(bytes), vg+"/"+name)
	return err
}
func (l *LVM) Remove(ctx context.Context, vg, name string) error {
	_, err := l.run(ctx, "lvremove", "--yes", vg+"/"+name)
	return err
}
func DevicePath(vg, lv string) string { return "/dev/" + vg + "/" + lv }
