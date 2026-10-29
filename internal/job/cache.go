package job

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/camilbenameur/gatehaul/internal/runner"
)

// CacheSchema is the version of dispatch.json and status.json.
const CacheSchema = 1

// Dispatch is what the controller decided before it opened the connection:
// enough to find the job again after a crash or a lost answer. It is written
// to jobs/<id>/dispatch.json before the submit. HostName is the host's own
// name (the codespace's random name): it tells a recreated host apart.
type Dispatch struct {
	Schema      int    `json:"schema"`
	ID          string `json:"id"`
	Repo        string `json:"repo"`
	Host        string `json:"host"` // display name, "gatehaul-<n>"
	HostNum     int    `json:"host_num"`
	HostID      string `json:"host_id"` // from the probe: detects a recreated host
	Ref         string `json:"ref"`
	HostName    string `json:"host_name,omitempty"`
	Key         string `json:"key"`
	SHA         string `json:"sha"`
	Kind        string `json:"kind"`
	Cmd         string `json:"cmd"`
	Fingerprint string `json:"fingerprint"`
	SubmittedAt string `json:"submitted_at"`

	// Top is the top level of the work tree the job was dispatched from (a gen
	// job only): its patch is applied there and nowhere else.
	Top string `json:"top,omitempty"`

	// AllowSensitive is set when gen ran with --allow-sensitive, so that wait
	// and apply keep the opt-in.
	AllowSensitive bool `json:"allow_sensitive,omitempty"`
}

// ErrNotFound means no cached job matches.
var ErrNotFound = errors.New("no such job")

// AmbiguousError means a prefix matches several jobs.
type AmbiguousError struct {
	Prefix     string
	Candidates []string
}

func (e *AmbiguousError) Error() string {
	return fmt.Sprintf("%q matches %d jobs: %s", e.Prefix, len(e.Candidates), strings.Join(e.Candidates, ", "))
}

// DefaultCacheDir is $GATEHAUL_CACHE_DIR, or gatehaul under the user cache
// directory.
func DefaultCacheDir(getenv func(string) string, userCacheDir func() (string, error)) (string, error) {
	if d := strings.TrimSpace(getenv("GATEHAUL_CACHE_DIR")); d != "" {
		return d, nil
	}
	base, err := userCacheDir()
	if err != nil {
		return "", fmt.Errorf("cannot find the user cache directory: %w", err)
	}
	return filepath.Join(base, "gatehaul"), nil
}

// Cache is the laptop's record of the jobs it dispatched.
type Cache struct {
	dir string
}

// NewCache returns a cache rooted at dir. Nothing is created until the first
// write.
func NewCache(dir string) *Cache { return &Cache{dir: dir} }

// Dir is the cache root. Other files of gatehaul, such as the up lock, live
// beside the jobs directory.
func (c *Cache) Dir() string { return c.dir }

func (c *Cache) jobDir(id string) string { return filepath.Join(c.dir, "jobs", id) }

// JobDir is jobs/<id>, the directory that holds the files of one job: the
// apply lock and the files saved before a patch is written belong there. id
// must come from a Dispatch.
func (c *Cache) JobDir(id string) string { return c.jobDir(id) }

// SaveDispatch writes dispatch.json for d.ID.
func (c *Cache) SaveDispatch(d Dispatch) error {
	if !runner.IDPattern.MatchString(d.ID) {
		return fmt.Errorf("invalid job id %q", d.ID)
	}
	d.Schema = CacheSchema
	return writeJSON(filepath.Join(c.jobDir(d.ID), "dispatch.json"), d)
}

// LoadDispatch reads dispatch.json for the exact id.
func (c *Cache) LoadDispatch(id string) (Dispatch, error) {
	if !runner.IDPattern.MatchString(id) {
		return Dispatch{}, ErrNotFound
	}
	var d Dispatch
	err := readJSON(filepath.Join(c.jobDir(id), "dispatch.json"), &d)
	if errors.Is(err, fs.ErrNotExist) {
		return Dispatch{}, ErrNotFound
	}
	return d, err
}

// SaveStatus records the last status seen for s.ID. A terminal status is never
// replaced by a non-terminal one: a late answer from a slow wait must not
// bring a finished job back to life.
func (c *Cache) SaveStatus(s runner.JobStatus) error {
	if !runner.IDPattern.MatchString(s.ID) {
		return fmt.Errorf("invalid job id %q", s.ID)
	}
	if old, ok, err := c.LoadStatus(s.ID); err == nil && ok && isTerminal(old.State) && !isTerminal(s.State) {
		return nil
	}
	s.Schema = CacheSchema
	return writeJSON(filepath.Join(c.jobDir(s.ID), "status.json"), s)
}

// LoadStatus returns the last saved status. ok is false when there is none.
func (c *Cache) LoadStatus(id string) (s runner.JobStatus, ok bool, err error) {
	err = readJSON(filepath.Join(c.jobDir(id), "status.json"), &s)
	if errors.Is(err, fs.ErrNotExist) {
		return runner.JobStatus{}, false, nil
	}
	if err != nil {
		return runner.JobStatus{}, false, err
	}
	return s, true, nil
}

// isTerminal is false for a state that does not parse, so that a damaged
// cache entry never blocks an update.
func isTerminal(state string) bool {
	st, err := Parse(state)
	return err == nil && st.Terminal()
}

// IsTerminal reports whether the cached status of id is final.
func (c *Cache) IsTerminal(id string) bool {
	s, ok, err := c.LoadStatus(id)
	return err == nil && ok && isTerminal(s.State)
}

// Remove forgets a job that was never accepted by a host.
func (c *Cache) Remove(id string) error {
	if !runner.IDPattern.MatchString(id) {
		return fmt.Errorf("invalid job id %q", id)
	}
	return os.RemoveAll(c.jobDir(id))
}

// List returns every dispatched job, ordered by creation time then host.
func (c *Cache) List() ([]Dispatch, error) {
	entries, err := os.ReadDir(filepath.Join(c.dir, "jobs"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var all []Dispatch
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		d, err := c.LoadDispatch(e.Name())
		if errors.Is(err, ErrNotFound) {
			continue // a directory without dispatch.json is not a job
		}
		if err != nil {
			return nil, fmt.Errorf("job %s: %w", e.Name(), err)
		}
		all = append(all, d)
	}
	sort.Slice(all, func(i, j int) bool {
		ui, uj := ULID(all[i].ID), ULID(all[j].ID)
		if ui != uj {
			return ui < uj
		}
		return all[i].ID < all[j].ID
	})
	return all, nil
}

// Resolve finds a job by its full id, or by a unique prefix of the whole id or
// of its ULID part (case-insensitive). A prefix that matches several jobs is an
// *AmbiguousError that lists them; none is ErrNotFound.
func (c *Cache) Resolve(prefix string) (Dispatch, error) {
	prefix = strings.ToUpper(strings.TrimSpace(prefix))
	if prefix == "" {
		return Dispatch{}, errEmptyPrefix
	}
	all, err := c.List()
	if err != nil {
		return Dispatch{}, err
	}
	var hits []Dispatch
	for _, d := range all {
		if d.ID == prefix {
			return d, nil
		}
		if strings.HasPrefix(d.ID, prefix) || strings.HasPrefix(ULID(d.ID), prefix) {
			hits = append(hits, d)
		}
	}
	switch len(hits) {
	case 0:
		return Dispatch{}, ErrNotFound
	case 1:
		return hits[0], nil
	}
	ids := make([]string, len(hits))
	for i, d := range hits {
		ids[i] = d.ID
	}
	return Dispatch{}, &AmbiguousError{Prefix: prefix, Candidates: ids}
}

// FindActive returns the unique job of repo at sha that is not known to be
// finished (no saved status counts as not finished). Several are an
// *AmbiguousError; none is ErrNotFound.
func (c *Cache) FindActive(repo, sha string) (Dispatch, error) {
	all, err := c.List()
	if err != nil {
		return Dispatch{}, err
	}
	var hits []Dispatch
	for _, d := range all {
		if d.Repo == repo && d.SHA == sha && !c.IsTerminal(d.ID) {
			hits = append(hits, d)
		}
	}
	return one(hits, "the pending jobs of "+shortSHA(sha))
}

// FindLatest returns the most recent job of repo at sha, finished or not.
func (c *Cache) FindLatest(repo, sha string) (Dispatch, error) {
	all, err := c.List()
	if err != nil {
		return Dispatch{}, err
	}
	var last *Dispatch
	for i := range all {
		if all[i].Repo == repo && all[i].SHA == sha {
			last = &all[i]
		}
	}
	if last == nil {
		return Dispatch{}, ErrNotFound
	}
	return *last, nil
}

// FindLatestGen returns the most recent gen job of repo at sha, whatever its
// state: the one whose patch "apply" brings back, once it has finished.
func (c *Cache) FindLatestGen(repo, sha string) (Dispatch, error) {
	all, err := c.List()
	if err != nil {
		return Dispatch{}, err
	}
	var last *Dispatch
	for i := range all {
		if all[i].Repo == repo && all[i].SHA == sha && all[i].Kind == runner.KindGen {
			last = &all[i]
		}
	}
	if last == nil {
		return Dispatch{}, ErrNotFound
	}
	return *last, nil
}

// Applied records that the patch of a gen job was applied to the working tree,
// so that nothing ever applies it twice. It is jobs/<id>/applied.json.
type Applied struct {
	Schema    int      `json:"schema"`
	SHA256    string   `json:"sha256"`
	Files     []string `json:"files"`
	AppliedAt string   `json:"applied_at"`
}

// SavePatch stores the patch of a gen job in jobs/<id>/patch.
func (c *Cache) SavePatch(id string, data []byte) error {
	if !runner.IDPattern.MatchString(id) {
		return fmt.Errorf("invalid job id %q", id)
	}
	return writeFile(filepath.Join(c.jobDir(id), "patch"), data)
}

// LoadPatch returns the stored patch. ok is false when there is none.
func (c *Cache) LoadPatch(id string) (data []byte, ok bool, err error) {
	if !runner.IDPattern.MatchString(id) {
		return nil, false, nil
	}
	data, err = os.ReadFile(filepath.Join(c.jobDir(id), "patch"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// SaveApplied records the application of the patch of job id.
func (c *Cache) SaveApplied(id string, a Applied) error {
	if !runner.IDPattern.MatchString(id) {
		return fmt.Errorf("invalid job id %q", id)
	}
	a.Schema = CacheSchema
	if a.Files == nil {
		a.Files = []string{}
	}
	return writeJSON(filepath.Join(c.jobDir(id), "applied.json"), a)
}

// LoadApplied returns the record of an applied patch. ok is false when the
// patch of the job was never applied.
func (c *Cache) LoadApplied(id string) (a Applied, ok bool, err error) {
	if !runner.IDPattern.MatchString(id) {
		return Applied{}, false, nil
	}
	err = readJSON(filepath.Join(c.jobDir(id), "applied.json"), &a)
	if errors.Is(err, fs.ErrNotExist) {
		return Applied{}, false, nil
	}
	if err != nil {
		return Applied{}, false, err
	}
	return a, true, nil
}

func one(hits []Dispatch, what string) (Dispatch, error) {
	switch len(hits) {
	case 0:
		return Dispatch{}, ErrNotFound
	case 1:
		return hits[0], nil
	}
	ids := make([]string, len(hits))
	for i, d := range hits {
		ids[i] = d.ID
	}
	return Dispatch{}, &AmbiguousError{Prefix: what, Candidates: ids}
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// writeJSON writes v to path atomically: a temporary file in the same
// directory, then a rename, so that a reader never sees half a file.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(b, '\n'))
}

// writeFile writes b to path atomically, the way writeJSON does.
func writeFile(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
