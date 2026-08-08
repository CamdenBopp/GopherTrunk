package main

// SDRTrunk playlist import.
//
// SDRTrunk (github.com/DSheirer/sdrtrunk) keeps its entire configuration in
// one XML "playlist" — by default $HOME/SDRTrunk/playlist/default.xml. That
// file holds both halves of what GopherTrunk needs:
//
//	<channel>  one per system+site: the control-channel frequencies, the
//	           decoder type (P25 P1/P2, DMR, LTR, MPT1327) and, for P25
//	           Phase 1, the modulation (C4FM vs CQPSK/LSM).
//	<alias>    the talkgroup / radio-ID catalogue, keyed by the alias-list
//	           name each channel references.
//
// This file maps that document onto the same parsedSystem values the PDF and
// CSV importers produce, so an SDRTrunk playlist flows through the existing
// review TUI and comment-preserving config merge unchanged.
//
// One playlist yields MANY systems (unlike a RadioReference PDF, which is one
// system per file), so the entry point returns a slice.

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// sdrtrunkMaxRangeExpand caps how many talkgroups a single <id
// type="talkgroupRange"> alias is expanded into. GopherTrunk's talkgroup CSV
// has no range row, so ranges become individual entries; a runaway range
// (0..65535 is legal in SDRTrunk) would otherwise bloat the CSV with tens of
// thousands of identical rows. Oversized ranges are skipped and reported.
const sdrtrunkMaxRangeExpand = 1024

// --- playlist XML schema (only the fields we consume) -----------------------

type stPlaylist struct {
	XMLName  xml.Name    `xml:"playlist"`
	Version  int         `xml:"version,attr"`
	Channels []stChannel `xml:"channel"`
	Aliases  []stAlias   `xml:"alias"`
}

type stChannel struct {
	System    string   `xml:"system,attr"`
	Site      string   `xml:"site,attr"`
	Name      string   `xml:"name,attr"`
	Enabled   string   `xml:"enabled,attr"`
	AliasList string   `xml:"alias_list_name"`
	Decode    stDecode `xml:"decode_configuration"`
	Source    stSource `xml:"source_configuration"`
}

// isEnabled reports whether the channel is set to auto-start in SDRTrunk.
// Only an explicit enabled="false" counts as disabled — an absent attribute
// is treated as enabled so playlists from older SDRTrunk versions aren't all
// demoted.
func (ch stChannel) isEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(ch.Enabled), "false")
}

type stDecode struct {
	Type       string `xml:"type,attr"`
	Modulation string `xml:"modulation,attr"`
}

// stSource covers both tuner source shapes: sourceConfigTuner carries a
// single frequency attribute, sourceConfigTunerMultipleFrequency a list of
// <frequency> child elements.
type stSource struct {
	Type        string   `xml:"type,attr"`
	Frequency   string   `xml:"frequency,attr"`
	Frequencies []string `xml:"frequency"`
}

type stAlias struct {
	Name  string      `xml:"name,attr"`
	List  string      `xml:"list,attr"`
	Group string      `xml:"group,attr"`
	IDs   []stAliasID `xml:"id"`
}

type stAliasID struct {
	Type     string `xml:"type,attr"`
	Protocol string `xml:"protocol,attr"`
	Value    string `xml:"value,attr"`
	Min      string `xml:"min,attr"`
	Max      string `xml:"max,attr"`
	Priority string `xml:"priority,attr"`
}

// --- entry point ------------------------------------------------------------

// parseSDRTrunkPlaylist reads an SDRTrunk playlist XML and returns one
// parsedSystem per distinct <channel system="…"> value, along with any
// non-fatal warnings (unsupported decoders, oversized ranges, …) worth
// printing to the operator.
func parseSDRTrunkPlaylist(path string) ([]parsedSystem, []string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("import: read SDRTrunk playlist %s: %w", path, err)
	}
	var pl stPlaylist
	if err := xml.Unmarshal(raw, &pl); err != nil {
		return nil, nil, fmt.Errorf("import: parse SDRTrunk playlist %s: %w", path, err)
	}
	if len(pl.Channels) == 0 {
		return nil, nil, fmt.Errorf("import: SDRTrunk playlist %s contains no <channel> entries", path)
	}

	var warnings []string
	warn := func(format string, a ...any) {
		warnings = append(warnings, fmt.Sprintf(format, a...))
	}

	// Pass 1 — group channels into systems. Channels sharing a system name
	// become sites of one system; channels sharing system+site merge (a site
	// is sometimes split across several channel rows).
	type sysAccum struct {
		sys          parsedSystem
		siteIdx      map[string]int  // lowercased site name → index into sys.Sites
		aliasLists   map[string]bool // lowercased alias-list names referenced
		demod        string          // p25 phase 1 demod mode, "" = default c4fm
		demodSet     bool            // a p25 phase 1 channel has set demod
		demodEnabled bool            // demod came from an enabled channel
		demodFrom    string          // channel that set demod, for conflict reporting
	}
	order := []string{}
	byName := map[string]*sysAccum{}

	for _, ch := range pl.Channels {
		proto, demod, ok := sdrtrunkProtocol(ch.Decode.Type, ch.Decode.Modulation)
		if !ok {
			warn("skipped channel %q: decoder %s is not a trunked protocol GopherTrunk decodes",
				sdrtrunkChannelLabel(ch), sdrtrunkDecoderLabel(ch.Decode.Type))
			continue
		}
		freqs := ch.Source.freqHz()
		if len(freqs) == 0 {
			warn("skipped channel %q: no tuner frequency in its source configuration (source type %q)",
				sdrtrunkChannelLabel(ch), ch.Source.Type)
			continue
		}

		sysName := strings.TrimSpace(ch.System)
		if sysName == "" {
			// A channel with no system name is still importable on its own.
			sysName = sdrtrunkChannelLabel(ch)
		}
		key := strings.ToLower(sysName)
		acc, seen := byName[key]
		if !seen {
			acc = &sysAccum{
				sys: parsedSystem{
					Name:       sysName,
					Protocol:   proto,
					SystemType: "SDRTrunk playlist",
					SourcePath: path,
				},
				siteIdx:    map[string]int{},
				aliasLists: map[string]bool{},
			}
			byName[key] = acc
			order = append(order, key)
		} else if acc.sys.Protocol != proto {
			warn("system %q: channel %q decodes %s but the system was already set to %s — keeping %s",
				sysName, sdrtrunkChannelLabel(ch), proto, acc.sys.Protocol, acc.sys.Protocol)
		}

		// P25 Phase 1 modulation is a per-system setting in GopherTrunk but a
		// per-channel one in SDRTrunk, and C4FM vs LSM is not derivable from
		// anything else. Precedence when channels disagree: an enabled
		// channel's modulation beats a disabled one's — the enabled channel
		// is the one the operator actually runs, so it reflects what locks
		// on air; a disabled channel is often a leftover experiment with the
		// other waveform. Ties within the same enabled state keep the first
		// channel and warn, since that conflict is not resolvable from the
		// playlist alone.
		if proto == "p25" {
			enabled := ch.isEnabled()
			switch {
			case !acc.demodSet:
				acc.demod, acc.demodSet, acc.demodEnabled, acc.demodFrom = demod, true, enabled, sdrtrunkChannelLabel(ch)
			case acc.demod != demod && enabled && !acc.demodEnabled:
				warn("system %q: importing as %s from enabled channel %q, overriding %s from disabled channel %q — fix p25_phase1_demod_mode by hand if that is wrong",
					sysName, sdrtrunkDemodLabel(demod), sdrtrunkChannelLabel(ch),
					sdrtrunkDemodLabel(acc.demod), acc.demodFrom)
				acc.demod, acc.demodEnabled, acc.demodFrom = demod, true, sdrtrunkChannelLabel(ch)
			case acc.demod != demod && enabled == acc.demodEnabled:
				warn("system %q: channel %q is %s but %q is %s — importing as %s; GopherTrunk sets the modulation per system, so split the sites into two systems or fix p25_phase1_demod_mode by hand if that is wrong",
					sysName, sdrtrunkChannelLabel(ch), sdrtrunkDemodLabel(demod),
					acc.demodFrom, sdrtrunkDemodLabel(acc.demod), sdrtrunkDemodLabel(acc.demod))
			case enabled && !acc.demodEnabled:
				// Same modulation but from an enabled channel — upgrade the
				// provenance so a later disagreeing disabled channel can't
				// win a tie against it.
				acc.demodEnabled, acc.demodFrom = true, sdrtrunkChannelLabel(ch)
			}
		}

		if al := strings.TrimSpace(ch.AliasList); al != "" {
			acc.aliasLists[strings.ToLower(al)] = true
		}

		siteName := strings.TrimSpace(ch.Site)
		if siteName == "" {
			siteName = strings.TrimSpace(ch.Name)
		}
		if siteName == "" {
			siteName = "(unnamed site)"
		}
		idx, ok := acc.siteIdx[strings.ToLower(siteName)]
		if !ok {
			acc.sys.Sites = append(acc.sys.Sites, parsedSite{
				SiteName: siteName,
				Include:  true,
			})
			idx = len(acc.sys.Sites) - 1
			acc.siteIdx[strings.ToLower(siteName)] = idx
		}
		site := &acc.sys.Sites[idx]
		for _, hz := range freqs {
			if sdrtrunkHasFreq(site.Frequencies, hz) {
				continue
			}
			// Every frequency on a trunked SDRTrunk channel is a control-
			// channel candidate — that is exactly what the multiple-frequency
			// source config means (rotate until one locks).
			site.Frequencies = append(site.Frequencies, parsedFreq{Hz: hz, ControlChannel: true})
		}
	}

	if len(order) == 0 {
		return nil, warnings, fmt.Errorf("import: SDRTrunk playlist %s has no trunked channels to import", path)
	}

	// Pass 2 — attach the alias catalogue. Aliases are scoped by alias-list
	// name, and several systems may share one list, so each system takes a
	// copy of every alias in the lists its channels reference.
	aliasesByList := map[string][]stAlias{}
	for _, a := range pl.Aliases {
		key := strings.ToLower(strings.TrimSpace(a.List))
		aliasesByList[key] = append(aliasesByList[key], a)
	}

	out := make([]parsedSystem, 0, len(order))
	for _, key := range order {
		acc := byName[key]
		acc.sys.P25DemodMode = acc.demod

		lists := make([]string, 0, len(acc.aliasLists))
		for l := range acc.aliasLists {
			lists = append(lists, l)
		}
		sort.Strings(lists)

		seenTG := map[uint32]bool{}
		seenRID := map[uint32]bool{}
		for _, l := range lists {
			group, ok := aliasesByList[l]
			if !ok {
				warn("system %q: alias list %q referenced by a channel but not present in the playlist", acc.sys.Name, l)
				continue
			}
			for _, a := range group {
				sdrtrunkApplyAlias(&acc.sys, a, seenTG, seenRID, warn)
			}
		}
		sort.Slice(acc.sys.Talkgroups, func(i, j int) bool {
			return acc.sys.Talkgroups[i].Dec < acc.sys.Talkgroups[j].Dec
		})
		sort.Slice(acc.sys.Radios, func(i, j int) bool {
			return acc.sys.Radios[i].Dec < acc.sys.Radios[j].Dec
		})
		out = append(out, acc.sys)
	}
	return out, warnings, nil
}

// sdrtrunkApplyAlias turns one <alias> into talkgroup and/or radio-ID rows on
// sys. An alias carries a name + group and any number of <id> children; the
// optional <id type="priority"> applies to every identifier in the alias.
func sdrtrunkApplyAlias(sys *parsedSystem, a stAlias, seenTG, seenRID map[uint32]bool, warn func(string, ...any)) {
	name := strings.TrimSpace(a.Name)
	group := strings.TrimSpace(a.Group)

	// SDRTrunk priority: -1 = DO_NOT_MONITOR, 0 = "selected" (i.e. unset),
	// 1..100 = explicit priority with lower numbers monitored first.
	priority, lockout := 0, false
	for _, id := range a.IDs {
		if !strings.EqualFold(id.Type, "priority") {
			continue
		}
		p, err := strconv.Atoi(strings.TrimSpace(id.Priority))
		if err != nil {
			continue
		}
		switch {
		case p < 0:
			lockout = true
		case p > 0:
			priority = p
		}
	}

	addTG := func(dec uint32) {
		if seenTG[dec] {
			return
		}
		seenTG[dec] = true
		sys.Talkgroups = append(sys.Talkgroups, parsedTalkgroup{
			Dec:      dec,
			Hex:      strings.ToUpper(strconv.FormatUint(uint64(dec), 16)),
			AlphaTag: name,
			Group:    group,
			Scan:     !lockout,
			Priority: priority,
			Lockout:  lockout,
		})
	}
	addRID := func(dec uint32) {
		if seenRID[dec] {
			return
		}
		seenRID[dec] = true
		sys.Radios = append(sys.Radios, parsedRadio{
			Dec:      dec,
			Alias:    name,
			Group:    group,
			Priority: priority,
			Lockout:  lockout,
		})
	}

	for _, id := range a.IDs {
		switch strings.ToLower(id.Type) {
		case "talkgroup", "p25fullyqualifiedtalkgroup":
			if dec, ok := sdrtrunkID(id.Value); ok {
				addTG(dec)
			}
		case "radio", "p25fullyqualifiedradio":
			if dec, ok := sdrtrunkID(id.Value); ok {
				addRID(dec)
			}
		case "talkgrouprange":
			sdrtrunkExpandRange(id, name, "talkgroup", addTG, warn)
		case "radiorange":
			sdrtrunkExpandRange(id, name, "radio", addRID, warn)
		}
	}
}

// sdrtrunkExpandRange expands a talkgroupRange / radioRange alias into
// individual rows, skipping (and reporting) ranges wider than
// sdrtrunkMaxRangeExpand.
func sdrtrunkExpandRange(id stAliasID, aliasName, kind string, add func(uint32), warn func(string, ...any)) {
	min, okMin := sdrtrunkID(id.Min)
	max, okMax := sdrtrunkID(id.Max)
	if !okMin || !okMax || max < min {
		return
	}
	if span := uint64(max) - uint64(min) + 1; span > sdrtrunkMaxRangeExpand {
		warn("alias %q: skipped %s range %d-%d (%d wide, cap is %d) — add it to the CSV by hand if you need it",
			aliasName, kind, min, max, span, sdrtrunkMaxRangeExpand)
		return
	}
	for v := min; ; v++ {
		add(v)
		if v == max {
			break
		}
	}
}

// sdrtrunkID parses an identifier attribute, rejecting blanks, negatives and
// anything outside uint32.
func sdrtrunkID(s string) (uint32, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(v), true
}

// freqHz returns every tuner frequency on a source configuration, covering
// both the single-frequency attribute and the multiple-frequency child list.
func (s stSource) freqHz() []uint32 {
	var out []uint32
	if hz, ok := sdrtrunkFreq(s.Frequency); ok {
		out = append(out, hz)
	}
	for _, f := range s.Frequencies {
		if hz, ok := sdrtrunkFreq(f); ok && !sdrtrunkHasHz(out, hz) {
			out = append(out, hz)
		}
	}
	return out
}

func sdrtrunkFreq(s string) (uint32, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil || v == 0 || v > 0xFFFFFFFF {
		return 0, false
	}
	return uint32(v), true
}

func sdrtrunkHasHz(list []uint32, hz uint32) bool {
	for _, v := range list {
		if v == hz {
			return true
		}
	}
	return false
}

func sdrtrunkHasFreq(list []parsedFreq, hz uint32) bool {
	for _, f := range list {
		if f.Hz == hz {
			return true
		}
	}
	return false
}

// sdrtrunkProtocol maps an SDRTrunk decode_configuration type onto a
// GopherTrunk trunking protocol, plus the p25_phase1_demod_mode implied by the
// channel's modulation. ok is false for decoders GopherTrunk has no trunking
// pipeline for (conventional NBFM/AM channels, Passport).
func sdrtrunkProtocol(decodeType, modulation string) (proto, demod string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(decodeType)) {
	case "decodeconfigp25", "decodeconfigp25phase1":
		// SDRTrunk's "CQPSK" is the LSM waveform; C4FM (and an absent
		// modulation) is the default FM-discriminator path.
		switch strings.ToUpper(strings.TrimSpace(modulation)) {
		case "CQPSK", "LSM":
			return "p25", "cqpsk", true
		default:
			return "p25", "", true
		}
	case "decodeconfigp25phase2":
		return "p25p2", "", true
	case "decodeconfigdmr":
		return "dmr", "", true
	case "decodeconfignxdn":
		return "nxdn", "", true
	case "decodeconfigltrnet", "decodeconfigltrstandard":
		return "ltr", "", true
	case "decodeconfigmpt1327":
		return "mpt1327", "", true
	default:
		return "", "", false
	}
}

// sdrtrunkDemodLabel names a demod mode for operator-facing messages, where
// the empty (default) value has to read as the modulation it actually means.
func sdrtrunkDemodLabel(demod string) string {
	if demod == "" {
		return "c4fm"
	}
	return demod
}

func sdrtrunkDecoderLabel(t string) string {
	if strings.TrimSpace(t) == "" {
		return "(none)"
	}
	return t
}

// sdrtrunkChannelLabel builds a human-readable "System / Site / Name" label
// for warnings, skipping the empty components.
func sdrtrunkChannelLabel(ch stChannel) string {
	parts := make([]string, 0, 3)
	for _, p := range []string{ch.System, ch.Site, ch.Name} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "(unnamed channel)"
	}
	return strings.Join(parts, " / ")
}

// --- playlist discovery -----------------------------------------------------

// resolveSDRTrunkPlaylist turns whatever the operator passed to -sdrtrunk into
// a playlist file path. It accepts:
//
//	""  / "auto"      discover SDRTrunk's default playlist directory
//	<file.xml>        the playlist itself
//	<dir>             a directory holding the playlist, or SDRTrunk's data
//	                  root (in which case its playlist/ subdirectory is used)
//	<SDRTrunk.app>    the macOS application bundle — which holds no user
//	                  data, so this falls back to the default location
func resolveSDRTrunkPlaylist(arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" || strings.EqualFold(arg, "auto") {
		return discoverSDRTrunkPlaylist()
	}
	info, err := os.Stat(arg)
	if err != nil {
		return "", fmt.Errorf("import: -sdrtrunk %s: %w", arg, err)
	}
	if !info.IsDir() {
		return arg, nil
	}
	// An application bundle is program code, never the operator's playlist —
	// treat pointing at it as "use my SDRTrunk data".
	if strings.EqualFold(filepath.Ext(arg), ".app") {
		path, err := discoverSDRTrunkPlaylist()
		if err != nil {
			return "", fmt.Errorf("import: %s is the application bundle, not your SDRTrunk data: %w", arg, err)
		}
		return path, nil
	}
	if p, ok := pickSDRTrunkPlaylistInDir(filepath.Join(arg, "playlist")); ok {
		return p, nil
	}
	if p, ok := pickSDRTrunkPlaylistInDir(arg); ok {
		return p, nil
	}
	return "", fmt.Errorf("import: no playlist XML found in %s (looked in %s too)", arg, filepath.Join(arg, "playlist"))
}

// discoverSDRTrunkPlaylist looks in SDRTrunk's default data roots. The
// packaged application stores everything under $HOME/SDRTrunk with the
// playlist in a playlist/ subdirectory (DirectoryPreference's
// getDirectoryPlaylist); operators running from a source checkout usually
// have the repo itself as the data root, conventionally $HOME/sdrtrunk. On a
// case-insensitive filesystem (macOS default) the two spellings are one
// directory and the second probe is a no-op; on case-sensitive filesystems
// they are genuinely distinct locations.
func discoverSDRTrunkPlaylist() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	dirs := []string{
		filepath.Join(home, "SDRTrunk", "playlist"),
		filepath.Join(home, "sdrtrunk", "playlist"),
	}
	for _, dir := range dirs {
		if p, ok := pickSDRTrunkPlaylistInDir(dir); ok {
			return p, nil
		}
	}
	return "", fmt.Errorf("no SDRTrunk playlist found in %s — pass -sdrtrunk <playlist.xml>", strings.Join(dirs, " or "))
}

// pickSDRTrunkPlaylistInDir chooses the playlist file in dir: SDRTrunk's own
// default names first, otherwise the sole .xml file if the directory is
// unambiguous. SDRTrunk's *.backup rotations are never chosen.
func pickSDRTrunkPlaylistInDir(dir string) (string, bool) {
	for _, name := range []string{"default.xml", "playlist.xml"} {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, true
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	var candidates []string
	for _, e := range entries {
		if e.IsDir() || !strings.EqualFold(filepath.Ext(e.Name()), ".xml") {
			continue
		}
		candidates = append(candidates, filepath.Join(dir, e.Name()))
	}
	if len(candidates) == 1 {
		return candidates[0], true
	}
	return "", false
}
