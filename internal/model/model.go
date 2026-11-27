// Package model contains the normalized, redacted bundle and the stable public
// report schema. The bundle is the only input to the analysis core.
package model

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

const (
	BundleSchema = "coldstart/bundle/v1"
	ReportSchema = "coldstart/v1"
)

// Bundle is a normalized snapshot of the Kubernetes and log inputs used by an
// analysis. Secret-bearing values are removed before a bundle is written.
type Bundle struct {
	Schema       string            `json:"schema"`
	Source       string            `json:"source,omitempty"`
	CapturedAt   time.Time         `json:"capturedAt"`
	Pod          corev1.Pod        `json:"pod"`
	Node         *corev1.Node      `json:"node,omitempty"`
	Events       []corev1.Event    `json:"events,omitempty"`
	Logs         []LogCapture      `json:"logs,omitempty"`
	OperatorPods []corev1.Pod      `json:"operatorPods,omitempty"`
	Probe        *ProbeResult      `json:"probe,omitempty"`
	Notes        []string          `json:"notes,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// LogCapture is one container log stream. Previous is true for kubectl's
// --previous stream. Lines have already been split and timestamped. Truncated
// indicates that the stream reached the collection byte cap.
type LogCapture struct {
	Container string    `json:"container"`
	Init      bool      `json:"init,omitempty"`
	Previous  bool      `json:"previous,omitempty"`
	Truncated bool      `json:"truncated,omitempty"`
	Lines     []LogLine `json:"lines"`
}

type LogLine struct {
	Timestamp time.Time `json:"timestamp"`
	Text      string    `json:"text"`
}

type ProbeResult struct {
	Endpoint      string    `json:"endpoint"`
	Model         string    `json:"model,omitempty"`
	RequestSentAt time.Time `json:"requestSentAt"`
	FirstTokenAt  time.Time `json:"firstTokenAt"`
	FirstToken    string    `json:"firstToken,omitempty"`
	StatusCode    int       `json:"statusCode,omitempty"`
	Error         string    `json:"error,omitempty"`
}

type EngineInfo struct {
	Name            string `json:"name"`
	Version         string `json:"version"`
	Container       string `json:"container,omitempty"`
	ContainerSource string `json:"containerSource,omitempty"`
	RulePack        string `json:"rulePack,omitempty"`
}

type Hardware struct {
	GPU       string `json:"gpu,omitempty"`
	Count     int    `json:"count"`
	Node      string `json:"node,omitempty"`
	NodeFresh bool   `json:"nodeFresh"`
	Storage   string `json:"storage,omitempty"`
}

type Phase struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	ParentID   string         `json:"parentId,omitempty"`
	Start      *time.Time     `json:"start"`
	End        *time.Time     `json:"end"`
	Secs       float64        `json:"secs"`
	Source     string         `json:"source"`
	Confidence string         `json:"confidence"`
	Attrs      map[string]any `json:"attrs,omitempty"`
}

type Attempt struct {
	Index       int            `json:"index"`
	Outcome     string         `json:"outcome"`
	Start       *time.Time     `json:"start"`
	End         *time.Time     `json:"end"`
	CrashReason string         `json:"crashReason,omitempty"`
	LostSecs    float64        `json:"lostSecs,omitempty"`
	Attrs       map[string]any `json:"attrs,omitempty"`
	Phases      []Phase        `json:"phases"`
}

type Finding struct {
	Phase              string  `json:"phase"`
	ObservedSecs       float64 `json:"observedSecs"`
	Condition          string  `json:"condition"`
	Recommendation     string  `json:"recommendation"`
	EstRecoverableSecs float64 `json:"recoverableSecs"`
	Confidence         string  `json:"confidence"`
	Tradeoff           string  `json:"tradeoff,omitempty"`
	Text               string  `json:"text"`
}

type RulePackInfo struct {
	Engine  string `json:"engine"`
	Version string `json:"version"`
	Path    string `json:"path"`
	Warning string `json:"warning,omitempty"`
}

type Report struct {
	Schema           string       `json:"schema"`
	Pod              string       `json:"pod"`
	Engine           EngineInfo   `json:"engine"`
	Hardware         Hardware     `json:"hardware"`
	T0               time.Time    `json:"t0"`
	ReadyAt          *time.Time   `json:"readyAt,omitempty"`
	EndAt            *time.Time   `json:"endAt"`
	TotalSecs        float64      `json:"totalSecs"`
	Phases           []Phase      `json:"phases"`
	Attempts         []Attempt    `json:"attempts"`
	UnattributedSecs float64      `json:"unattributedSecs"`
	Findings         []Finding    `json:"findings"`
	Warnings         []string     `json:"warnings"`
	RulePack         RulePackInfo `json:"rulePack"`
	Probe            *ProbeResult `json:"probe,omitempty"`
	CapturedAt       time.Time    `json:"capturedAt"`
}
