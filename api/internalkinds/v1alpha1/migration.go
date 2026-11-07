package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// MigrationItem is the durable outcome of one destination write.
type MigrationItem struct {
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace,omitempty"`
	Name            string `json:"name"`
	Result          string `json:"result"`
	Error           string `json:"error,omitempty"`
	ErrorKey        string `json:"errorKey,omitempty"`
	Hint            string `json:"hint,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

// MigrationZone tracks authority without retaining source records or credentials.
type MigrationZone struct {
	Name                string   `json:"name"`
	Namespace           string   `json:"namespace,omitempty"`
	ZoneName            string   `json:"zoneName"`
	UID                 string   `json:"uid,omitempty"`
	OriginalNameservers []string `json:"originalNameservers" nullable:"false"`
	OriginalPrimary     string   `json:"originalPrimary"`
	Nameservers         []string `json:"nameservers" nullable:"false"`
	Primary             string   `json:"primary"`
}

// MigrationCutover records an authority replacement attempt, including failures.
type MigrationCutover struct {
	StartedAt   metav1.Time     `json:"startedAt"`
	FinishedAt  *metav1.Time    `json:"finishedAt,omitempty"`
	Actor       string          `json:"actor"`
	Status      string          `json:"status"`
	Nameservers []string        `json:"nameservers" nullable:"false"`
	Addresses   []string        `json:"addresses,omitempty"`
	Items       []MigrationItem `json:"items" nullable:"false"`
}

// MigrationRunSpec is a credential-free journal of an API import and its cutovers.
type MigrationRunSpec struct {
	Source            string             `json:"source"`
	SourceURL         string             `json:"sourceURL,omitempty"`
	Actor             string             `json:"actor"`
	StartedAt         metav1.Time        `json:"startedAt"`
	FinishedAt        *metav1.Time       `json:"finishedAt,omitempty"`
	Status            string             `json:"status"`
	Items             []MigrationItem    `json:"items" nullable:"false"`
	Zones             []MigrationZone    `json:"zones" nullable:"false"`
	Warnings          []string           `json:"warnings" nullable:"false"`
	SettingsError     string             `json:"settingsError,omitempty"`
	SettingsPersisted *bool              `json:"settingsPersisted,omitempty"`
	JournalError      string             `json:"journalError,omitempty"`
	Failure           string             `json:"failure,omitempty"`
	Deadline          *metav1.Time       `json:"deadline,omitempty"`
	Cutovers          []MigrationCutover `json:"cutovers" nullable:"false"`
}

// MigrationRun is an internal journal stored as a row or a Kubernetes Secret.
// +kubebuilder:object:root=true
type MigrationRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MigrationRunSpec `json:"spec"`
}

// MigrationRunList is a list of journals.
// +kubebuilder:object:root=true
type MigrationRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MigrationRun `json:"items" nullable:"false"`
}

// Validate implements the internal store validation contract.
func (r *MigrationRun) Validate() error {
	errs := validateMeta(KindMigrationRun, r.Namespace, r.Name)
	if r.Spec.StartedAt.IsZero() {
		errs.add("spec.startedAt", "required")
	}
	switch r.Spec.Source {
	case "technitium", "pihole", "adguard", "blocky", "manual", "existing":
	default:
		errs.add("spec.source", "unsupported source")
	}
	switch r.Spec.Status {
	case "running", "completed", "partial":
	default:
		errs.add("spec.status", "unsupported status")
	}
	return errs.err(KindMigrationRun, r.Namespace, r.Name)
}
