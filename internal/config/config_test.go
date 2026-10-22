package config

import (
	"strings"
	"testing"
	"time"
)

const validConfig = `storage:
  root: /backups
  expectedDiskID: disk-1
  keepSuccessful: 3
targets:
  - name: primary
    host: postgres.default.svc
    majorVersion: 17
    username: backup
    databases: [app, "Unicode_数据库"]
    passwordFile: /secrets/password
`

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse(strings.NewReader(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	target := cfg.Targets[0]
	if target.Port != 5432 || target.SSLMode != "verify-full" || target.Timeout != time.Hour {
		t.Fatalf("unexpected defaults: %+v", target)
	}
}

func TestRejectInvalidConfiguration(t *testing.T) {
	for name, text := range map[string]string{
		"unknown":                 validConfig + "surprise: true\n",
		"unknown target":          strings.Replace(validConfig, "    host:", "    typo: x\n    host:", 1),
		"duplicate YAML":          validConfig + "targets: []\n",
		"multiple documents":      validConfig + "---\n" + validConfig,
		"empty":                   "",
		"relative root":           strings.Replace(validConfig, "/backups", "backups", 1),
		"root filesystem":         strings.Replace(validConfig, "/backups", "/", 1),
		"unsafe name":             strings.Replace(validConfig, "name: primary", "name: ../escape", 1),
		"unsupported version":     strings.Replace(validConfig, "majorVersion: 17", "majorVersion: 15", 1),
		"empty databases":         strings.Replace(validConfig, `[app, "Unicode_数据库"]`, "[]", 1),
		"duplicate database":      strings.Replace(validConfig, `[app, "Unicode_数据库"]`, "[app, app]", 1),
		"newline host":            strings.Replace(validConfig, "postgres.default.svc", `"evil\nhost"`, 1),
		"libpq connection string": strings.Replace(validConfig, `[app, "Unicode_数据库"]`, `["host=evil"]`, 1),
		"invalid ssl":             validConfig + "    sslMode: invalid\n",
		"zero timeout":            validConfig + "    timeout: 0s\n",
		"huge timeout":            validConfig + "    timeout: 25h\n",
		"zero retention":          strings.Replace(validConfig, "keepSuccessful: 3", "keepSuccessful: 0", 1),
		"zero port":               validConfig + "    port: 0\n",
		"duplicate target":        validConfig + strings.Split(validConfig, "targets:\n")[1],
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(strings.NewReader(text)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
