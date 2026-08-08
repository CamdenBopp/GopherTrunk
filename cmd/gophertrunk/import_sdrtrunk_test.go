package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// samplePlaylist is a trimmed SDRTrunk playlist exercising every shape the
// importer handles: two sites of one P25 system with a shared frequency pair,
// a second (DMR) system on its own alias list, a conventional NBFM channel
// that must be skipped, talkgroup / radio / range / priority alias IDs.
const samplePlaylist = `<?xml version="1.0" encoding="UTF-8"?>
<playlist version="4">
  <channel system="County P25" enabled="false" site="North" order="1" name="Control">
    <alias_list_name>County</alias_list_name>
    <source_configuration type="sourceConfigTunerMultipleFrequency" source_type="TUNER_MULTIPLE_FREQUENCIES">
      <frequency>774231250</frequency>
      <frequency>774831250</frequency>
    </source_configuration>
    <decode_configuration type="decodeConfigP25Phase1" modulation="CQPSK" traffic_channel_pool_size="20"/>
  </channel>
  <channel system="County P25" enabled="true" site="South" order="1" name="South CC">
    <alias_list_name>County</alias_list_name>
    <source_configuration type="sourceConfigTuner" frequency="770000000" source_type="TUNER"/>
    <decode_configuration type="decodeConfigP25Phase1" modulation="CQPSK"/>
  </channel>
  <channel system="County P25" enabled="true" site="South" order="2" name="South CC alt">
    <alias_list_name>County</alias_list_name>
    <source_configuration type="sourceConfigTuner" frequency="770000000" source_type="TUNER"/>
    <decode_configuration type="decodeConfigP25Phase1" modulation="CQPSK"/>
  </channel>
  <channel system="City DMR" enabled="true" site="Downtown" name="Downtown">
    <alias_list_name>City</alias_list_name>
    <source_configuration type="sourceConfigTuner" frequency="851012500" source_type="TUNER"/>
    <decode_configuration type="decodeConfigDMR"/>
  </channel>
  <channel system="Weather" enabled="true" name="NOAA">
    <source_configuration type="sourceConfigTuner" frequency="162550000" source_type="TUNER"/>
    <decode_configuration type="decodeConfigNBFM"/>
  </channel>
  <alias list="County" group="Fire" color="0" name="FD01DISP">
    <id type="talkgroup" protocol="APCO25" value="5000"/>
  </alias>
  <alias list="County" group="Fire" color="0" name="FD01TAC">
    <id type="talkgroup" protocol="APCO25" value="5001"/>
    <id type="priority" priority="3"/>
  </alias>
  <alias list="County" group="Data" color="0" name="IGNORE ME">
    <id type="talkgroup" protocol="APCO25" value="5002"/>
    <id type="priority" priority="-1"/>
  </alias>
  <alias list="County" group="Units" color="0" name="ENGINE 1">
    <id type="radio" protocol="APCO25" value="900007"/>
  </alias>
  <alias list="County" group="Ops" color="0" name="OPS BLOCK">
    <id type="talkgroupRange" protocol="APCO25" min="6000" max="6002"/>
  </alias>
  <alias list="County" group="Huge" color="0" name="WHOLE BAND">
    <id type="talkgroupRange" protocol="APCO25" min="1" max="65535"/>
  </alias>
  <alias list="City" group="PW" color="0" name="PUBLIC WORKS">
    <id type="talkgroup" protocol="DMR" value="101"/>
  </alias>
</playlist>
`

func writeSamplePlaylist(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "default.xml")
	if err := os.WriteFile(path, []byte(samplePlaylist), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func findSystem(t *testing.T, systems []parsedSystem, name string) parsedSystem {
	t.Helper()
	for _, s := range systems {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("system %q not found in %d parsed systems", name, len(systems))
	return parsedSystem{}
}

func TestParseSDRTrunkPlaylist_GroupsChannelsIntoSystems(t *testing.T) {
	systems, _, err := parseSDRTrunkPlaylist(writeSamplePlaylist(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// The conventional NBFM channel is not a trunked system.
	if len(systems) != 2 {
		names := make([]string, len(systems))
		for i, s := range systems {
			names[i] = s.Name
		}
		t.Fatalf("want 2 systems, got %d: %v", len(systems), names)
	}

	p25 := findSystem(t, systems, "County P25")
	if p25.Protocol != "p25" {
		t.Errorf("protocol = %q, want p25", p25.Protocol)
	}
	if p25.P25DemodMode != "cqpsk" {
		t.Errorf("demod mode = %q, want cqpsk", p25.P25DemodMode)
	}
	// Two distinct sites; the two "South" channels merge into one.
	if len(p25.Sites) != 2 {
		t.Fatalf("want 2 sites, got %d", len(p25.Sites))
	}
	south := p25.Sites[1]
	if south.SiteName != "South" {
		t.Errorf("site[1] = %q, want South", south.SiteName)
	}
	if len(south.Frequencies) != 1 {
		t.Errorf("South frequencies = %v, want the duplicate collapsed to one", south.Frequencies)
	}

	// Every channel frequency is a control-channel candidate, and each site
	// is included by default so a -no-tui import is usable as-is.
	ccs := collectControlChannels(p25)
	want := []uint32{770000000, 774231250, 774831250}
	if fmt.Sprint(ccs) != fmt.Sprint(want) {
		t.Errorf("control channels = %v, want %v", ccs, want)
	}

	dmr := findSystem(t, systems, "City DMR")
	if dmr.Protocol != "dmr" {
		t.Errorf("DMR protocol = %q", dmr.Protocol)
	}
	if dmr.P25DemodMode != "" {
		t.Errorf("DMR system carries a P25 demod mode %q", dmr.P25DemodMode)
	}
}

func TestParseSDRTrunkPlaylist_AliasesScopedByAliasList(t *testing.T) {
	systems, _, err := parseSDRTrunkPlaylist(writeSamplePlaylist(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p25 := findSystem(t, systems, "County P25")
	dmr := findSystem(t, systems, "City DMR")

	// The City alias list must not leak into the County system.
	for _, tg := range p25.Talkgroups {
		if tg.Dec == 101 {
			t.Fatalf("talkgroup 101 from alias list \"City\" leaked into County P25")
		}
	}
	if len(dmr.Talkgroups) != 1 || dmr.Talkgroups[0].Dec != 101 {
		t.Fatalf("City DMR talkgroups = %+v, want just 101", dmr.Talkgroups)
	}

	byDec := map[uint32]parsedTalkgroup{}
	for _, tg := range p25.Talkgroups {
		byDec[tg.Dec] = tg
	}

	plain, ok := byDec[5000]
	if !ok {
		t.Fatal("talkgroup 5000 missing")
	}
	if plain.AlphaTag != "FD01DISP" || plain.Group != "Fire" {
		t.Errorf("5000 = %+v, want alpha FD01DISP / group Fire", plain)
	}
	if !plain.Scan || plain.Lockout || plain.Priority != 0 {
		t.Errorf("5000 flags = %+v, want scanned, unlocked, no priority", plain)
	}
	if plain.Hex != "1388" {
		t.Errorf("5000 hex = %q, want 1388", plain.Hex)
	}

	if prio := byDec[5001]; prio.Priority != 3 || !prio.Scan || prio.Lockout {
		t.Errorf("5001 = %+v, want priority 3 and scanned", prio)
	}
	// SDRTrunk's priority -1 is DO_NOT_MONITOR.
	if muted := byDec[5002]; !muted.Lockout || muted.Scan {
		t.Errorf("5002 = %+v, want locked out and not scanned", muted)
	}

	// A small talkgroupRange expands to individual rows...
	for _, dec := range []uint32{6000, 6001, 6002} {
		tg, ok := byDec[dec]
		if !ok {
			t.Errorf("range talkgroup %d missing", dec)
			continue
		}
		if tg.AlphaTag != "OPS BLOCK" {
			t.Errorf("%d alpha = %q, want OPS BLOCK", dec, tg.AlphaTag)
		}
	}
	// ...while an oversized one is skipped rather than exploding the CSV.
	if _, ok := byDec[40000]; ok {
		t.Error("oversized talkgroupRange was expanded")
	}

	if len(p25.Radios) != 1 || p25.Radios[0].Dec != 900007 || p25.Radios[0].Alias != "ENGINE 1" {
		t.Fatalf("radios = %+v, want RID 900007 / ENGINE 1", p25.Radios)
	}
}

func TestParseSDRTrunkPlaylist_WarnsOnSkippedContent(t *testing.T) {
	_, warnings, err := parseSDRTrunkPlaylist(writeSamplePlaylist(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "decodeConfigNBFM") {
		t.Errorf("no warning for the skipped conventional channel:\n%s", joined)
	}
	if !strings.Contains(joined, "WHOLE BAND") {
		t.Errorf("no warning for the oversized range:\n%s", joined)
	}
}

// TestParseSDRTrunkPlaylist_WarnsOnModulationConflict covers the case a real
// playlist hits: two sites of one system decoding at different modulations.
// GopherTrunk's p25_phase1_demod_mode is per system, so one has to lose — the
// operator must be told which, including when the loser is the C4FM default.
func TestParseSDRTrunkPlaylist_WarnsOnModulationConflict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "default.xml")
	const mixed = `<playlist version="4">
  <channel system="Mixed" site="North" name="Control">
    <source_configuration type="sourceConfigTuner" frequency="774231250"/>
    <decode_configuration type="decodeConfigP25Phase1" modulation="CQPSK"/>
  </channel>
  <channel system="Mixed" site="South" name="South CC">
    <source_configuration type="sourceConfigTuner" frequency="770000000"/>
    <decode_configuration type="decodeConfigP25Phase1" modulation="C4FM"/>
  </channel>
</playlist>`
	if err := os.WriteFile(path, []byte(mixed), 0o644); err != nil {
		t.Fatal(err)
	}
	systems, warnings, err := parseSDRTrunkPlaylist(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := systems[0].P25DemodMode; got != "cqpsk" {
		t.Errorf("demod = %q, want the first channel's cqpsk", got)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "c4fm") || !strings.Contains(joined, "cqpsk") {
		t.Errorf("modulation conflict not reported, warnings:\n%s", joined)
	}
}

// TestParseSDRTrunkPlaylist_EnabledChannelModulationWins pins the precedence
// rule verified on air against a real MARCS-IP capture: the operator's
// enabled channel (C4FM, locks) outranks a disabled leftover experiment
// (CQPSK, zero sync hits) even when the disabled channel comes first in the
// playlist. Both orderings are covered, plus the tie-upgrade: an enabled
// channel that AGREES with an earlier disabled one must also take over
// provenance, so a later disagreeing disabled channel cannot override it.
func TestParseSDRTrunkPlaylist_EnabledChannelModulationWins(t *testing.T) {
	dir := t.TempDir()

	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Disabled CQPSK first, enabled C4FM second — the shape of the real
	// playlist this rule was built for.
	p := write("disabled-first.xml", `<playlist version="4">
  <channel system="MARCS" enabled="false" site="Cambridge North" name="Control">
    <source_configuration type="sourceConfigTuner" frequency="774231250"/>
    <decode_configuration type="decodeConfigP25Phase1" modulation="CQPSK"/>
  </channel>
  <channel system="MARCS" enabled="true" site="Byesville" name="Byesville">
    <source_configuration type="sourceConfigTuner" frequency="774831250"/>
    <decode_configuration type="decodeConfigP25Phase1" modulation="C4FM"/>
  </channel>
</playlist>`)
	systems, warnings, err := parseSDRTrunkPlaylist(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := systems[0].P25DemodMode; got != "" {
		t.Errorf("demod = %q, want the enabled channel's c4fm default (empty)", got)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "overriding") || !strings.Contains(joined, "disabled channel") {
		t.Errorf("override not reported, warnings:\n%s", joined)
	}

	// Enabled CQPSK first, disabled C4FM second — enabled still wins, and
	// the losing disabled channel is ignored without a conflict warning.
	p = write("enabled-first.xml", `<playlist version="4">
  <channel system="MARCS" enabled="true" site="North" name="Control">
    <source_configuration type="sourceConfigTuner" frequency="774231250"/>
    <decode_configuration type="decodeConfigP25Phase1" modulation="CQPSK"/>
  </channel>
  <channel system="MARCS" enabled="false" site="South" name="South CC">
    <source_configuration type="sourceConfigTuner" frequency="774831250"/>
    <decode_configuration type="decodeConfigP25Phase1" modulation="C4FM"/>
  </channel>
</playlist>`)
	systems, warnings, err = parseSDRTrunkPlaylist(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := systems[0].P25DemodMode; got != "cqpsk" {
		t.Errorf("demod = %q, want the enabled channel's cqpsk", got)
	}
	for _, w := range warnings {
		if strings.Contains(w, "importing as") {
			t.Errorf("unexpected modulation warning when the enabled channel already won: %s", w)
		}
	}

	// Disabled CQPSK, then enabled CQPSK (agrees — provenance upgrades),
	// then disabled C4FM (disagrees — must NOT win the tie).
	p = write("provenance-upgrade.xml", `<playlist version="4">
  <channel system="MARCS" enabled="false" site="A" name="A">
    <source_configuration type="sourceConfigTuner" frequency="774231250"/>
    <decode_configuration type="decodeConfigP25Phase1" modulation="CQPSK"/>
  </channel>
  <channel system="MARCS" enabled="true" site="B" name="B">
    <source_configuration type="sourceConfigTuner" frequency="774831250"/>
    <decode_configuration type="decodeConfigP25Phase1" modulation="CQPSK"/>
  </channel>
  <channel system="MARCS" enabled="false" site="C" name="C">
    <source_configuration type="sourceConfigTuner" frequency="775000000"/>
    <decode_configuration type="decodeConfigP25Phase1" modulation="C4FM"/>
  </channel>
</playlist>`)
	systems, _, err = parseSDRTrunkPlaylist(p)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := systems[0].P25DemodMode; got != "cqpsk" {
		t.Errorf("demod = %q, want cqpsk kept after provenance upgrade", got)
	}
}

func TestParseSDRTrunkPlaylist_RejectsUntrunkedPlaylist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "default.xml")
	const conventionalOnly = `<playlist version="4">
  <channel system="Weather" name="NOAA">
    <source_configuration type="sourceConfigTuner" frequency="162550000"/>
    <decode_configuration type="decodeConfigNBFM"/>
  </channel>
</playlist>`
	if err := os.WriteFile(path, []byte(conventionalOnly), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := parseSDRTrunkPlaylist(path); err == nil {
		t.Fatal("want an error when a playlist holds no trunked channels")
	}
}

func TestSDRTrunkProtocolMapping(t *testing.T) {
	cases := []struct {
		decode, modulation string
		proto, demod       string
		ok                 bool
	}{
		{"decodeConfigP25Phase1", "C4FM", "p25", "", true},
		{"decodeConfigP25Phase1", "CQPSK", "p25", "cqpsk", true},
		{"decodeConfigP25Phase1", "", "p25", "", true},
		{"decodeConfigP25", "LSM", "p25", "cqpsk", true},
		{"decodeConfigP25Phase2", "", "p25p2", "", true},
		{"decodeConfigDMR", "", "dmr", "", true},
		{"decodeConfigLTRNet", "", "ltr", "", true},
		{"decodeConfigLTRStandard", "", "ltr", "", true},
		{"decodeConfigMPT1327", "", "mpt1327", "", true},
		{"decodeConfigNBFM", "", "", "", false},
		{"decodeConfigPassport", "", "", "", false},
		{"", "", "", "", false},
	}
	for _, c := range cases {
		proto, demod, ok := sdrtrunkProtocol(c.decode, c.modulation)
		if proto != c.proto || demod != c.demod || ok != c.ok {
			t.Errorf("%s/%s → (%q,%q,%v), want (%q,%q,%v)",
				c.decode, c.modulation, proto, demod, ok, c.proto, c.demod, c.ok)
		}
	}
}

// TestParseSDRTrunkPlaylist_MergesIntoConfig proves the playlist reaches
// config.yaml through the shared merge path, including the two fields only
// this importer supplies: rid_alias_file and p25_phase1_demod_mode.
func TestParseSDRTrunkPlaylist_MergesIntoConfig(t *testing.T) {
	systems, _, err := parseSDRTrunkPlaylist(writeSamplePlaylist(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	res, err := mergeIntoConfig(systems, mergeOptions{ConfigPath: cfgPath})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	merged := string(res.ConfigYAML)
	for _, want := range []string{"County P25", "City DMR", "p25_phase1_demod_mode: cqpsk", "rid_alias_file:"} {
		if !strings.Contains(merged, want) {
			t.Errorf("merged config missing %q:\n%s", want, merged)
		}
	}
	// Only the system that actually has radio IDs gets a RID file.
	var ridFiles int
	for _, c := range res.CSVs {
		if strings.Contains(filepath.Base(c.Path), "rids-") {
			ridFiles++
			if !strings.Contains(string(c.Data), "900007,ENGINE 1") {
				t.Errorf("RID CSV %s missing the radio alias row:\n%s", c.Path, c.Data)
			}
		}
	}
	if ridFiles != 1 {
		t.Errorf("wrote %d RID CSVs, want 1", ridFiles)
	}
	for _, c := range res.CSVs {
		if _, err := os.Stat(c.Path); err != nil {
			t.Errorf("CSV not written: %v", err)
		}
	}
}

func TestResolveSDRTrunkPlaylist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	playlistDir := filepath.Join(home, "SDRTrunk", "playlist")
	if err := os.MkdirAll(playlistDir, 0o755); err != nil {
		t.Fatal(err)
	}
	def := filepath.Join(playlistDir, "default.xml")
	if err := os.WriteFile(def, []byte(samplePlaylist), 0o644); err != nil {
		t.Fatal(err)
	}
	// A .backup rotation sits next to it and must never be chosen.
	if err := os.WriteFile(def+".backup", []byte(samplePlaylist), 0o644); err != nil {
		t.Fatal(err)
	}
	// The macOS app bundle holds program code, not the operator's playlist.
	appBundle := filepath.Join(home, "Applications", "SDRTrunk.app")
	if err := os.MkdirAll(appBundle, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, arg string
	}{
		{"empty discovers default", ""},
		{"auto discovers default", "auto"},
		{"explicit file", def},
		{"playlist directory", playlistDir},
		{"data root", filepath.Join(home, "SDRTrunk")},
		{"app bundle falls back to data", appBundle},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveSDRTrunkPlaylist(c.arg)
			if err != nil {
				t.Fatalf("resolve(%q): %v", c.arg, err)
			}
			if got != def {
				t.Errorf("resolve(%q) = %q, want %q", c.arg, got, def)
			}
		})
	}

	if _, err := resolveSDRTrunkPlaylist(filepath.Join(home, "nope.xml")); err == nil {
		t.Error("want an error for a missing path")
	}
}

// TestResolveSDRTrunkPlaylist_LowercaseSourceCheckout covers the operator who
// runs SDRTrunk from a source checkout: the data root is the repo itself,
// conventionally ~/sdrtrunk. On case-insensitive filesystems this spelling is
// the same directory as ~/SDRTrunk and either probe finds it; the test pins
// down that discovery also works where the spellings are distinct.
func TestResolveSDRTrunkPlaylist_LowercaseSourceCheckout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	playlistDir := filepath.Join(home, "sdrtrunk", "playlist")
	if err := os.MkdirAll(playlistDir, 0o755); err != nil {
		t.Fatal(err)
	}
	def := filepath.Join(playlistDir, "default.xml")
	if err := os.WriteFile(def, []byte(samplePlaylist), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := resolveSDRTrunkPlaylist("auto")
	if err != nil {
		t.Fatalf("resolve(auto): %v", err)
	}
	// Compare via os.Stat identity rather than string equality: on a
	// case-insensitive filesystem the probe may find the file under the
	// other capitalization of the same directory.
	wantInfo, err := os.Stat(def)
	if err != nil {
		t.Fatal(err)
	}
	gotInfo, err := os.Stat(got)
	if err != nil {
		t.Fatalf("resolved path %q: %v", got, err)
	}
	if !os.SameFile(wantInfo, gotInfo) {
		t.Errorf("resolve(auto) = %q, want %q", got, def)
	}

	// The source checkout passed explicitly as a directory works too.
	got, err = resolveSDRTrunkPlaylist(filepath.Join(home, "sdrtrunk"))
	if err != nil {
		t.Fatalf("resolve(~/sdrtrunk): %v", err)
	}
	if got != def {
		t.Errorf("resolve(~/sdrtrunk) = %q, want %q", got, def)
	}
}
