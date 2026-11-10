// Package schema defines the Go types that ARE the preflight report.
//
// Every struct here is closed: a field reaches the ConfigMap, the log lines or
// the receiver only if it is declared below with a json tag. The JSON Schema in
// schema/report.schema.json mirrors these types with additionalProperties:
// false at every level, and the tests in this package keep the two in sync and
// validate a full sample report against it. The one open leaf is
// Finding.Old/New: a rendered command/args item is a string on a correct
// chart, but 0.17.x could retype it (int, bool, map), and the report shows
// exactly what each chart rendered.
//
// Versioning: renaming, removing or retyping a field bumps SchemaVersion;
// adding an optional field does not.
package schema

// SchemaVersion of the report document.
const SchemaVersion = 1

// Per-app statuses, a strict ladder: AppError > AppDiff > AppUnknown > AppOK
// (AppOKNoopDiff counts as ok); AppSkipped only when nothing else exists.
const (
	AppOK         = "ok"
	AppOKNoopDiff = "ok-noop-diff"
	AppDiff       = "diff"
	AppError      = "error"
	AppUnknown    = "unknown"
	AppSkipped    = "skipped"
)

// Per-ApplicationSet and per-run statuses. The gate before a bump is
// GroupOK; GroupUnknown is never a verdict.
const (
	GroupOK      = "ok"
	GroupDiff    = "diff"
	GroupError   = "error"
	GroupUnknown = "unknown"
	GroupSkipped = "skipped"
)

// Chart source types an ApplicationSet may use.
const (
	SourceHelm = "helm"
	SourceOCI  = "oci"
	SourceGit  = "git"
)

// Finding categories (the 0.18.0 table) and impacts.
const (
	CategoryNullItem         = "null_item"
	CategoryDoubleQuoted     = "double_quoted"
	CategoryCommentTruncated = "comment_truncated"
	CategoryLeadingHash      = "leading_hash"
	CategoryPadded           = "padded"
	CategoryRetyped          = "retyped"
	CategoryLengthChanged    = "length_changed"
	CategoryOther            = "other"

	ImpactNone  = "none"
	ImpactRoll  = "roll"
	ImpactBreak = "break"
)

// Container types and fields that are compared.
const (
	ContainersKey     = "containers"
	InitContainersKey = "initContainers"
	FieldCommand      = "command"
	FieldArgs         = "args"
)

// Report is the whole document: one run, every ApplicationSet, every app.
type Report struct {
	SchemaVersion int        `json:"schema_version"`
	Run           Run        `json:"run"`
	Summary       RunSummary `json:"summary"`
	AppSets       []AppSet   `json:"appsets"`
}

// Run identifies the run and carries its verdict.
type Run struct {
	ID               string  `json:"id"`
	CaptainDomain    string  `json:"captain_domain"`
	TargetRevision   *string `json:"target_revision"`
	ArgocdCLIVersion *string `json:"argocd_cli_version"`
	AgentVersion     string  `json:"agent_version"`
	AgentCommit      string  `json:"agent_commit"`
	StartedAt        string  `json:"started_at"`
	FinishedAt       string  `json:"finished_at"`
	Status           string  `json:"status"`
}

// RunSummary counts ApplicationSets by status. AppsIgnored is the number of
// Applications in the Argo CD namespace that do not use the app chart and
// belong to no app-chart set (platform apps): seen, never checked.
type RunSummary struct {
	AppSets     int `json:"appsets"`
	OK          int `json:"ok"`
	Diff        int `json:"diff"`
	Error       int `json:"error"`
	Unknown     int `json:"unknown"`
	Skipped     int `json:"skipped"`
	AppsIgnored int `json:"apps_ignored"`
}

// AppSet is one ApplicationSet (or the synthetic "standalone" group) with its
// template pin — the thing an operator bumps — and its apps.
type AppSet struct {
	Name            string     `json:"name"`
	SourceType      *string    `json:"source_type"`
	CurrentRevision *string    `json:"current_revision"`
	TargetRevision  *string    `json:"target_revision"`
	Status          string     `json:"status"`
	Reason          *string    `json:"reason"`
	Summary         AppSummary `json:"summary"`
	Apps            []App      `json:"apps"`
}

// AppSummary counts apps by status.
type AppSummary struct {
	Apps       int `json:"apps"`
	OK         int `json:"ok"`
	OKNoopDiff int `json:"ok-noop-diff"`
	Diff       int `json:"diff"`
	Error      int `json:"error"`
	Unknown    int `json:"unknown"`
	Skipped    int `json:"skipped"`
}

// App is one Application's verdict. Namespace is the destination namespace
// (where the pods run). ValuesFiles are the chart source's valueFiles with the
// "$values/" ref stripped, in precedence order, so a tenant knows which files
// to look at. Diff is keyed by "Kind/name"; OtherChanges lists pod-template
// paths that differ outside command/args (paths only, no values).
type App struct {
	Name            string                  `json:"name"`
	Namespace       string                  `json:"namespace"`
	CurrentRevision *string                 `json:"current_revision"`
	Status          string                  `json:"status"`
	SyncStatus      *string                 `json:"sync_status"`
	Reason          *string                 `json:"reason"`
	ValuesFiles     []string                `json:"values_files"`
	Diff            map[string]ResourceDiff `json:"diff"`
	OtherChanges    map[string][]string     `json:"other_changes"`
	ArgocdDiff      *string                 `json:"argocd_diff"`
}

// ResourceDiff groups findings by container type, then container name.
type ResourceDiff struct {
	Containers     map[string]ContainerDiff `json:"containers,omitempty"`
	InitContainers map[string]ContainerDiff `json:"initContainers,omitempty"`
}

// ContainerDiff groups findings by field, then by list index (as a string).
type ContainerDiff struct {
	Command map[string]Finding `json:"command,omitempty"`
	Args    map[string]Finding `json:"args,omitempty"`
}

// Finding is one command/args item that renders differently at the target
// revision. Old/New are the rendered values verbatim: a string, or on 0.17.x
// a retyped scalar/map; nil when the index is absent on that side or the item
// is null.
type Finding struct {
	Old      any    `json:"old"`
	New      any    `json:"new"`
	Category string `json:"category"`
	Impact   string `json:"impact"`
	Hint     string `json:"hint"`
}

// Str returns a pointer to s, for the nullable string fields.
func Str(s string) *string { return &s }

// NewApp returns an App with every collection non-nil, so the JSON always
// carries [] / {} rather than null (the schema requires arrays and objects).
func NewApp(name, namespace string) App {
	return App{Name: name, Namespace: namespace, Status: AppOK, ValuesFiles: []string{}, Diff: map[string]ResourceDiff{}, OtherChanges: map[string][]string{}}
}
