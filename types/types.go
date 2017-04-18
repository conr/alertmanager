// Copyright 2015 Prometheus Team
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package types

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/common/model"
)

type state uint8

const (
	Unprocessed state = iota
	Active
	Silenced
	Inhibited
)

// allows tracking of the status of an alert (active, silenced, )
type AlertStatus struct {
	status state
	value  []string
}

// custom serializer for alertStatus that handles providing default value
// if alertStatus for given fingerprint is missing from the statusMap
func (a *AlertStatus) MarshalJSON() ([]byte, error) {
	status, found := statusMap[a.status]
	if !found {
		status = "unknown"
	}

	// TODO: Do we want to return all values?
	var value string
	if len(a.value) != 0 {
		value = a.value[0]
	}
	return json.Marshal(map[string]string{
		"status": status,
		"value":  value,
	})
}

var statusMap = map[state]string{
	Unprocessed: "unprocessed",
	Active:      "active",
	Silenced:    "silenced",
	Inhibited:   "inhibited",
}

// Marker helps to mark alerts as silenced and/or inhibited.
// All methods are goroutine-safe.
type Marker interface {
	SetStatus(model.Fingerprint, state, ...string) error
	Status(alert model.Fingerprint) AlertStatus

	Unprocessed(model.Fingerprint) bool
	Active(model.Fingerprint) bool
	Silenced(model.Fingerprint) ([]string, bool)
	Inhibited(model.Fingerprint) bool
}

// NewMarker returns an instance of a Marker implementation.
func NewMarker() Marker {
	return &memMarker{
		m: map[model.Fingerprint]*AlertStatus{},
	}
}

type memMarker struct {
	m map[model.Fingerprint]*AlertStatus

	mtx sync.RWMutex
}

// helper method for updating status so that Set(Inhibited|Silenced) can focus
// just on setting right values
func (m *memMarker) SetStatus(alert model.Fingerprint, s state, v ...string) error {
	m.mtx.Lock()
	defer m.mtx.Unlock()

	var (
		err         error
		status, prs = m.m[alert]
		state       state
	)

	if !prs {
		m.m[alert] = &AlertStatus{
			status: s,
			value:  v,
		}
		return nil
	}

	switch s {
	case Unprocessed, Active, Inhibited:
		setStatus(status, s)
	case Silenced:
		// Inhibited has a higher priority than Silenced.
		if status.status == Inhibited {
			break
		}

		state = s
		if len(v) == 0 {
			// If there are no silences then the alert is Active.
			state = Active
		}

		setStatus(status, state, v...)
	default:
		err = fmt.Errorf("unknown state: %d", s)
	}

	return err
}

func setStatus(status *AlertStatus, s state, v ...string) {
	status.status = s
	status.value = v
}

func (m *memMarker) Status(alert model.Fingerprint) AlertStatus {
	m.mtx.RLock()
	defer m.mtx.RUnlock()

	s, found := m.m[alert]
	if !found {
		return AlertStatus{}
	}
	return *s

}

func (m *memMarker) Unprocessed(alert model.Fingerprint) bool {
	s := m.Status(alert)
	return s.status == Unprocessed
}

func (m *memMarker) Active(alert model.Fingerprint) bool {
	s := m.Status(alert)
	return s.status == Active
}

func (m *memMarker) Inhibited(alert model.Fingerprint) bool {
	s := m.Status(alert)
	return s.status == Inhibited
}

func (m *memMarker) Silenced(alert model.Fingerprint) ([]string, bool) {
	s := m.Status(alert)
	return s.value, s.status == Silenced
}

// MultiError contains multiple errors and implements the error interface. Its
// zero value is ready to use. All its methods are goroutine safe.
type MultiError struct {
	mtx    sync.Mutex
	errors []error
}

// Add adds an error to the MultiError.
func (e *MultiError) Add(err error) {
	e.mtx.Lock()
	defer e.mtx.Unlock()

	e.errors = append(e.errors, err)
}

// Len returns the number of errors added to the MultiError.
func (e *MultiError) Len() int {
	e.mtx.Lock()
	defer e.mtx.Unlock()

	return len(e.errors)
}

// Errors returns the errors added to the MuliError. The returned slice is a
// copy of the internal slice of errors.
func (e *MultiError) Errors() []error {
	e.mtx.Lock()
	defer e.mtx.Unlock()

	return append(make([]error, 0, len(e.errors)), e.errors...)
}

func (e *MultiError) Error() string {
	e.mtx.Lock()
	defer e.mtx.Unlock()

	es := make([]string, 0, len(e.errors))
	for _, err := range e.errors {
		es = append(es, err.Error())
	}
	return strings.Join(es, "; ")
}

// Alert wraps a model.Alert with additional information relevant
// to internal of the Alertmanager.
// The type is never exposed to external communication and the
// embedded alert has to be sanitized beforehand.
type Alert struct {
	model.Alert

	// The authoritative timestamp.
	UpdatedAt    time.Time
	Timeout      bool
	WasSilenced  bool `json:"-"`
	WasInhibited bool `json:"-"`
}

// AlertSlice is a sortable slice of Alerts.
type AlertSlice []*Alert

func (as AlertSlice) Less(i, j int) bool { return as[i].UpdatedAt.Before(as[j].UpdatedAt) }
func (as AlertSlice) Swap(i, j int)      { as[i], as[j] = as[j], as[i] }
func (as AlertSlice) Len() int           { return len(as) }

// Alerts turns a sequence of internal alerts into a list of
// exposable model.Alert structures.
func Alerts(alerts ...*Alert) model.Alerts {
	res := make(model.Alerts, 0, len(alerts))
	for _, a := range alerts {
		v := a.Alert
		// If the end timestamp was set as the expected value in case
		// of a timeout but is not reached yet, do not expose it.
		if a.Timeout && !a.Resolved() {
			v.EndsAt = time.Time{}
		}
		res = append(res, &v)
	}
	return res
}

// Merge merges the timespan of two alerts based and overwrites annotations
// based on the authoritative timestamp.  A new alert is returned, the labels
// are assumed to be equal.
func (a *Alert) Merge(o *Alert) *Alert {
	// Let o always be the younger alert.
	if o.UpdatedAt.Before(a.UpdatedAt) {
		return o.Merge(a)
	}

	res := *o

	// Always pick the earliest starting time.
	if a.StartsAt.Before(o.StartsAt) {
		res.StartsAt = a.StartsAt
	}

	// A non-timeout resolved timestamp always rules.
	// The latest explicit resolved timestamp wins.
	if a.EndsAt.After(o.EndsAt) && !a.Timeout {
		res.EndsAt = a.EndsAt
	}

	return &res
}

// A Muter determines whether a given label set is muted.
type Muter interface {
	Mutes(model.LabelSet) bool
}

// A MuteFunc is a function that implements the Muter interface.
type MuteFunc func(model.LabelSet) bool

// Mutes implements the Muter interface.
func (f MuteFunc) Mutes(lset model.LabelSet) bool { return f(lset) }

// A Silence determines whether a given label set is muted.
type Silence struct {
	// A unique identifier across all connected instances.
	ID string `json:"id"`
	// A set of matchers determining if a label set is affect
	// by the silence.
	Matchers Matchers `json:"matchers"`

	// Time range of the silence.
	//
	// * StartsAt must not be before creation time
	// * EndsAt must be after StartsAt
	// * Deleting a silence means to set EndsAt to now
	// * Time range must not be modified in different ways
	//
	// TODO(fabxc): this may potentially be extended by
	// creation and update timestamps.
	StartsAt time.Time `json:"startsAt"`
	EndsAt   time.Time `json:"endsAt"`

	// The last time the silence was updated.
	UpdatedAt time.Time `json:"updatedAt"`

	// Information about who created the silence for which reason.
	CreatedBy string `json:"createdBy"`
	Comment   string `json:"comment,omitempty"`

	// timeFunc provides the time against which to evaluate
	// the silence. Used for test injection.
	now func() time.Time
}

// Validate returns true iff all fields of the silence have valid values.
func (s *Silence) Validate() error {
	if s.ID == "" {
		return fmt.Errorf("ID missing")
	}
	if len(s.Matchers) == 0 {
		return fmt.Errorf("at least one matcher required")
	}
	for _, m := range s.Matchers {
		if err := m.Validate(); err != nil {
			return fmt.Errorf("invalid matcher: %s", err)
		}
	}
	if s.StartsAt.IsZero() {
		return fmt.Errorf("start time missing")
	}
	if s.EndsAt.IsZero() {
		return fmt.Errorf("end time missing")
	}
	if s.EndsAt.Before(s.StartsAt) {
		return fmt.Errorf("start time must be before end time")
	}
	if s.CreatedBy == "" {
		return fmt.Errorf("creator information missing")
	}
	if s.Comment == "" {
		return fmt.Errorf("comment missing")
	}
	// if s.CreatedAt.IsZero() {
	//	return fmt.Errorf("creation timestamp missing")
	// }
	return nil
}

// Init initializes a silence. Must be called before using Mutes.
func (s *Silence) Init() error {
	for _, m := range s.Matchers {
		if err := m.Init(); err != nil {
			return err
		}
	}
	sort.Sort(s.Matchers)
	return nil
}

// Mutes implements the Muter interface.
//
// TODO(fabxc): consider making this a function accepting a
// timestamp and returning a Muter, i.e. s.Muter(ts).Mutes(lset).
func (s *Silence) Mutes(lset model.LabelSet) bool {
	var now time.Time
	if s.now != nil {
		now = s.now()
	} else {
		now = time.Now()
	}
	if now.Before(s.StartsAt) || now.After(s.EndsAt) {
		return false
	}
	return s.Matchers.Match(lset)
}

// Deleted returns whether a silence is deleted. Semantically this means it had no effect
// on history at any point.
func (s *Silence) Deleted() bool {
	return s.StartsAt.Equal(s.EndsAt)
}
