package dockerx

type ConnectSpec struct {
	Host          string `json:"host"`
	TLSCertPath   string `json:"tls_cert_path,omitempty"`
	TLSKeyPath    string `json:"tls_key_path,omitempty"`
	TLSCAPath     string `json:"tls_ca_path,omitempty"`
	APIVersion    string `json:"api_version,omitempty"`
	KeyPath       string `json:"key_path,omitempty"`
	KeyPassphrase string `json:"key_passphrase,omitempty"`
	UseAgent      bool   `json:"use_agent,omitempty"`
	Password      string `json:"password,omitempty"`
	// KnownHostsPath / SSHInsecure apply only to ssh:// hosts.
	KnownHostsPath string `json:"known_hosts_path,omitempty"`
	SSHInsecure    bool   `json:"ssh_insecure,omitempty"`
	Persistent     bool   `json:"persistent,omitempty"`
}
