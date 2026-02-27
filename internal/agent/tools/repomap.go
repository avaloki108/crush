package tools

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/fsext"
)

//go:embed repomap.md
var repomapDescription []byte

const (
	RepomapToolName   = "repomap"
	defaultMaxSymbols = 300
	maxRepomapTokens  = 8000
)

// RepomapParams configures what the repo map covers.
type RepomapParams struct {
	// Path is the root directory to map. Defaults to the working dir.
	Path string `json:"path,omitempty" description:"Root directory to map. Defaults to the working directory."`
	// Focus restricts output to files matching this substring (e.g. \"contracts/\", \".sol\").
	Focus string `json:"focus,omitempty" description:"Only include files whose path contains this substring. Useful to zoom in on contracts/, src/, etc."`
	// MaxSymbols caps how many symbols are shown per file. Defaults to 20.
	MaxSymbols int `json:"max_symbols,omitempty" description:"Maximum symbols to show per file. Default 20."`
}

// symbol is a code definition extracted from a source file.
type symbol struct {
	name     string
	kind     string // function, contract, event, error, modifier, struct, enum, interface, etc.
	line     int
	exported bool
}

// fileEntry holds a file path with its extracted symbols and a relevance score.
type fileEntry struct {
	relPath string
	absPath string
	modTime time.Time
	symbols []symbol
	score   float64
}

// NewRepomapTool builds a fantasy.AgentTool that generates a compact repository map.
// It tries universal-ctags first, then falls back to regex-based extraction.
func NewRepomapTool(workingDir string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		RepomapToolName,
		string(repomapDescription),
		func(ctx context.Context, params RepomapParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			root := params.Path
			if root == "" {
				root = workingDir
			}

			maxSym := params.MaxSymbols
			if maxSym <= 0 {
				maxSym = 20
			}

			entries, err := collectEntries(root, params.Focus)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("repomap: %v", err)), nil
			}

			// Score and sort: recently-modified and exported symbols rank higher.
			scoreEntries(entries)
			sort.Slice(entries, func(i, j int) bool {
				return entries[i].score > entries[j].score
			})

			out := renderMap(entries, root, maxSym)

			// Hard-cap output size so we don't blow the context window.
			if len(out) > maxRepomapTokens*4 {
				out = out[:maxRepomapTokens*4] + "\n... (truncated)"
			}

			return fantasy.NewTextResponse(out), nil
		})
}

// collectEntries walks root, extracting symbols from every recognized source file.
func collectEntries(root, focus string) ([]fileEntry, error) {
	// Try ctags first for best accuracy.
	ctagsEntries, err := collectWithCtags(root, focus)
	if err == nil && len(ctagsEntries) > 0 {
		return ctagsEntries, nil
	}
	// Fall back to regex-based extraction.
	return collectWithRegex(root, focus)
}

// ─── ctags-based extraction ───────────────────────────────────────────────────

type ctagsEntry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Kind     string `json:"kind"`
	Language string `json:"language"`
}

func collectWithCtags(root, focus string) ([]fileEntry, error) {
	ctagsPath, err := exec.LookPath("ctags")
	if err != nil {
		return nil, fmt.Errorf("ctags not found")
	}

	args := []string{
		"--output-format=json",
		"--fields=+n",
		"--recurse",
		"--languages=Go,Python,JavaScript,TypeScript,C,C++,Java,Ruby,Rust,Solidity,Kotlin,Scala,Swift",
		"--exclude=.git",
		"--exclude=node_modules",
		"--exclude=vendor",
		"--exclude=.crush",
		"-f", "-",
		root,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, ctagsPath, args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ctags error: %v", err)
	}

	fileMap := map[string]*fileEntry{}

	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || line[0] == '!' {
			continue
		}
		var ce ctagsEntry
		if err := json.Unmarshal(line, &ce); err != nil {
			continue
		}
		if focus != "" && !strings.Contains(ce.Path, focus) {
			continue
		}

		fe, ok := fileMap[ce.Path]
		if !ok {
			fi, err := os.Stat(ce.Path)
			var modTime time.Time
			if err == nil {
				modTime = fi.ModTime()
			}
			rel, _ := filepath.Rel(root, ce.Path)
			entry := &fileEntry{
				relPath: rel,
				absPath: ce.Path,
				modTime: modTime,
			}
			fileMap[ce.Path] = entry
			fe = entry
		}

		fe.symbols = append(fe.symbols, symbol{
			name:     ce.Name,
			kind:     ce.Kind,
			line:     ce.Line,
			exported: isExported(ce.Name),
		})
	}

	entries := make([]fileEntry, 0, len(fileMap))
	for _, fe := range fileMap {
		entries = append(entries, *fe)
	}
	return entries, nil
}

// ─── regex-based extraction ───────────────────────────────────────────────────

var langPatterns = map[string][]*langPattern{
	".sol": {
		{kind: "contract", re: regexp.MustCompile(`(?m)^\s*(abstract\s+)?(contract|interface|library)\s+(\w+)`)},
		{kind: "function", re: regexp.MustCompile(`(?m)^\s*function\s+(\w+)\s*\(`)},
		{kind: "event", re: regexp.MustCompile(`(?m)^\s*event\s+(\w+)\s*\(`)},
		{kind: "error", re: regexp.MustCompile(`(?m)^\s*error\s+(\w+)\s*\(`)},
		{kind: "modifier", re: regexp.MustCompile(`(?m)^\s*modifier\s+(\w+)\s*[\({]`)},
		{kind: "struct", re: regexp.MustCompile(`(?m)^\s*struct\s+(\w+)\s*\{`)},
		{kind: "enum", re: regexp.MustCompile(`(?m)^\s*enum\s+(\w+)\s*\{`)},
	},
	".go": {
		{kind: "func", re: regexp.MustCompile(`(?m)^func\s+(\([^)]+\)\s+)?(\w+)\s*\(`)},
		{kind: "type", re: regexp.MustCompile(`(?m)^type\s+(\w+)\s+(struct|interface)`)},
	},
	".py": {
		{kind: "class", re: regexp.MustCompile(`(?m)^class\s+(\w+)[\s(:]`)},
		{kind: "def", re: regexp.MustCompile(`(?m)^def\s+(\w+)\s*\(`)},
	},
	".ts": {
		{kind: "class", re: regexp.MustCompile(`(?m)^\s*(export\s+)?(abstract\s+)?class\s+(\w+)`)},
		{kind: "function", re: regexp.MustCompile(`(?m)^\s*(export\s+)?(async\s+)?function\s+(\w+)\s*[\(<]`)},
		{kind: "interface", re: regexp.MustCompile(`(?m)^\s*(export\s+)?interface\s+(\w+)`)},
		{kind: "type", re: regexp.MustCompile(`(?m)^\s*(export\s+)?type\s+(\w+)\s*=`)},
	},
	".js": {
		{kind: "class", re: regexp.MustCompile(`(?m)^\s*(export\s+)?(default\s+)?class\s+(\w+)`)},
		{kind: "function", re: regexp.MustCompile(`(?m)^\s*(export\s+)?(async\s+)?function\s+(\w+)\s*\(`)},
	},
	".rs": {
		{kind: "fn", re: regexp.MustCompile(`(?m)^\s*(pub\s+)?(async\s+)?fn\s+(\w+)\s*[<(]`)},
		{kind: "struct", re: regexp.MustCompile(`(?m)^\s*(pub\s+)?struct\s+(\w+)`)},
		{kind: "enum", re: regexp.MustCompile(`(?m)^\s*(pub\s+)?enum\s+(\w+)`)},
		{kind: "trait", re: regexp.MustCompile(`(?m)^\s*(pub\s+)?trait\s+(\w+)`)},
	},
	".java": {
		{kind: "class", re: regexp.MustCompile(`(?m)^\s*(public|private|protected)?\s*(abstract\s+)?class\s+(\w+)`)},
		{kind: "method", re: regexp.MustCompile(`(?m)^\s*(public|private|protected|static|\s)+[\w<>\[\]]+\s+(\w+)\s*\(`)},
		{kind: "interface", re: regexp.MustCompile(`(?m)^\s*(public\s+)?interface\s+(\w+)`)},
	},
}

type langPattern struct {
	kind string
	re   *regexp.Regexp
}

// captureGroupName extracts the symbol name from a regex match.
// Uses the last non-empty captured group (index > 0).
func captureGroupName(match []string) string {
	for i := len(match) - 1; i > 0; i-- {
		if match[i] != "" && !strings.HasPrefix(match[i], " ") {
			return strings.TrimSpace(match[i])
		}
	}
	return ""
}

func extractSymbols(content, ext string) []symbol {
	patterns, ok := langPatterns[ext]
	if !ok {
		return nil
	}

	var syms []symbol
	lines := strings.Split(content, "\n")

	for _, pat := range patterns {
		matches := pat.re.FindAllStringSubmatchIndex(content, -1)
		for _, loc := range matches {
			full := pat.re.FindStringSubmatch(content[loc[0]:loc[1]])
			if full == nil {
				continue
			}
			name := captureGroupName(full)
			if name == "" {
				continue
			}

			// Find line number by counting newlines up to the match start.
			lineNum := strings.Count(content[:loc[0]], "\n") + 1
			_ = lines

			syms = append(syms, symbol{
				name:     name,
				kind:     pat.kind,
				line:     lineNum,
				exported: isExported(name),
			})
		}
	}

	// Deduplicate by name+kind.
	seen := map[string]bool{}
	out := syms[:0]
	for _, s := range syms {
		key := s.kind + ":" + s.name
		if !seen[key] {
			seen[key] = true
			out = append(out, s)
		}
	}
	return out
}

func collectWithRegex(root, focus string) ([]fileEntry, error) {
	walker := fsext.NewFastGlobWalker(root)
	var entries []fileEntry

	knownExts := map[string]bool{}
	for ext := range langPatterns {
		knownExts[ext] = true
	}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			if info != nil && info.IsDir() && walker.ShouldSkip(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if walker.ShouldSkip(path) {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if !knownExts[ext] {
			return nil
		}
		if focus != "" && !strings.Contains(path, focus) {
			return nil
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		syms := extractSymbols(string(content), ext)
		if len(syms) == 0 {
			return nil
		}

		rel, _ := filepath.Rel(root, path)
		entries = append(entries, fileEntry{
			relPath: rel,
			absPath: path,
			modTime: info.ModTime(),
			symbols: syms,
		})
		return nil
	})
	return entries, err
}

// ─── scoring ──────────────────────────────────────────────────────────────────

func scoreEntries(entries []fileEntry) {
	if len(entries) == 0 {
		return
	}

	// Normalise mod time: newest file gets score 1.0, oldest 0.0.
	var newest, oldest time.Time
	for _, e := range entries {
		if oldest.IsZero() || e.modTime.Before(oldest) {
			oldest = e.modTime
		}
		if e.modTime.After(newest) {
			newest = e.modTime
		}
	}

	span := newest.Sub(oldest).Seconds()

	for i := range entries {
		e := &entries[i]

		var timeFactor float64
		if span > 0 {
			timeFactor = e.modTime.Sub(oldest).Seconds() / span
		}

		// Count exported symbols.
		exported := 0
		for _, s := range e.symbols {
			if s.exported {
				exported++
			}
		}
		exportFactor := float64(exported) / float64(max(len(e.symbols), 1))

		// Depth penalty: deeper paths are less likely to be entry points.
		depth := strings.Count(e.relPath, string(os.PathSeparator))
		depthPenalty := 1.0 / float64(depth+1)

		e.score = 0.4*timeFactor + 0.4*exportFactor + 0.2*depthPenalty
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ─── rendering ────────────────────────────────────────────────────────────────

func renderMap(entries []fileEntry, root string, maxSymPerFile int) string {
	var sb strings.Builder
	sb.WriteString("# Repository Map\n\n")

	rootInfo, _ := os.Stat(root)
	if rootInfo != nil {
		sb.WriteString(fmt.Sprintf("Root: %s\n\n", root))
	}

	if len(entries) == 0 {
		sb.WriteString("(no symbols found)\n")
		return sb.String()
	}

	// Group by directory.
	type dirGroup struct {
		dir     string
		entries []fileEntry
	}

	dirMap := map[string]*dirGroup{}
	var dirOrder []string

	for _, e := range entries {
		dir := filepath.Dir(e.relPath)
		if dir == "." {
			dir = ""
		}
		if _, ok := dirMap[dir]; !ok {
			dirMap[dir] = &dirGroup{dir: dir}
			dirOrder = append(dirOrder, dir)
		}
		dirMap[dir].entries = append(dirMap[dir].entries, e)
	}

	totalFiles := 0
	for _, dirKey := range dirOrder {
		grp := dirMap[dirKey]

		label := grp.dir
		if label == "" {
			label = filepath.Base(root)
		}
		sb.WriteString(fmt.Sprintf("## %s/\n", label))

		for _, e := range grp.entries {
			base := filepath.Base(e.relPath)
			sb.WriteString(fmt.Sprintf("  %s\n", base))

			syms := e.symbols
			if len(syms) > maxSymPerFile {
				syms = syms[:maxSymPerFile]
			}

			// Group symbols by kind.
			kindOrder := []string{"contract", "interface", "library", "class", "struct", "enum", "trait", "type", "function", "func", "def", "method", "fn", "event", "error", "modifier"}
			kindMap := map[string][]symbol{}
			for _, s := range syms {
				kindMap[s.kind] = append(kindMap[s.kind], s)
			}

			for _, kind := range kindOrder {
				ss, ok := kindMap[kind]
				if !ok {
					continue
				}
				for _, s := range ss {
					indicator := ""
					if !s.exported {
						indicator = "~"
					}
					sb.WriteString(fmt.Sprintf("    %s%s %s\n", indicator, kind, s.name))
				}
			}
			// Catch any kinds not in kindOrder.
			for kind, ss := range kindMap {
				known := false
				for _, k := range kindOrder {
					if k == kind {
						known = true
						break
					}
				}
				if !known {
					for _, s := range ss {
						sb.WriteString(fmt.Sprintf("    %s %s\n", kind, s.name))
					}
				}
			}
		}

		sb.WriteString("\n")
		totalFiles++
	}

	sb.WriteString(fmt.Sprintf("---\n%d files mapped\n", len(entries)))
	return sb.String()
}

// isExported returns true if the identifier starts with an uppercase letter,
// or (for Solidity/Python/JS) is not prefixed with _ or is a Public function.
func isExported(name string) bool {
	if name == "" {
		return false
	}
	c := name[0]
	if c >= 'A' && c <= 'Z' {
		return true
	}
	if c == '_' {
		return false
	}
	// Lower-case but not underscore-prefixed: treat as exported for most languages.
	return true
}
