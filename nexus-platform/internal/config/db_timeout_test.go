package config

import (
	"strings"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
)

// TestApplyDBTimeouts_InjectsAndPreservesParams verifies ApplyDBTimeouts sets readTimeout/
// writeTimeout (from queryTimeout) and timeout (from connAcquireTimeout) while preserving the
// pre-existing DSN params (parseTime, charset, loc). Re-parsing the output is the assertion so
// the test is independent of go-sql-driver's param ordering/formatting.
func TestApplyDBTimeouts_InjectsAndPreservesParams(t *testing.T) {
	in := "root:@tcp(127.0.0.1:3306)/nexus_platform?charset=utf8mb4&parseTime=True&loc=Local"
	out := ApplyDBTimeouts(in, 10*time.Second, 5*time.Second)

	cfg, err := gomysql.ParseDSN(out)
	if err != nil {
		t.Fatalf("ParseDSN(%q): %v", out, err)
	}
	if cfg.ReadTimeout != 10*time.Second {
		t.Errorf("ReadTimeout = %v, want 10s", cfg.ReadTimeout)
	}
	if cfg.WriteTimeout != 10*time.Second {
		t.Errorf("WriteTimeout = %v, want 10s", cfg.WriteTimeout)
	}
	if cfg.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s", cfg.Timeout)
	}
	// Pre-existing params survive.
	if !cfg.ParseTime {
		t.Errorf("ParseTime lost; want true")
	}
	if cfg.DBName != "nexus_platform" {
		t.Errorf("DBName = %q, want nexus_platform", cfg.DBName)
	}
	if cfg.Params["charset"] != "utf8mb4" {
		t.Errorf("charset = %q, want utf8mb4", cfg.Params["charset"])
	}
}

// TestApplyDBTimeouts_NonPositiveSkips verifies a ≤0 duration leaves that knob untouched
// (zero value), so an operator can disable a timeout via env without it being forced on.
func TestApplyDBTimeouts_NonPositiveSkips(t *testing.T) {
	in := "root:@tcp(127.0.0.1:3306)/nexus_platform?parseTime=True"
	out := ApplyDBTimeouts(in, 0, 0)
	cfg, err := gomysql.ParseDSN(out)
	if err != nil {
		t.Fatalf("ParseDSN(%q): %v", out, err)
	}
	if cfg.ReadTimeout != 0 || cfg.WriteTimeout != 0 || cfg.Timeout != 0 {
		t.Errorf("timeouts should be unset; got read=%v write=%v dial=%v", cfg.ReadTimeout, cfg.WriteTimeout, cfg.Timeout)
	}
}

// TestApplyDBTimeouts_ReportsFoundRows verifies the pool DSN asks MySQL for FOUND (matched) rows,
// not CHANGED rows (queue #91): without CLIENT_FOUND_ROWS an UPDATE that re-writes identical
// values reports 0 affected rows, and every repository mapping `RowsAffected()==0` to not-found
// answered a second identical "Lưu" with 404. The flag must be set whether or not the timeout
// knobs are enabled — it is a semantic of the DSN, not of the timeouts — and an explicit
// clientFoundRows=false in DATABASE_URL is overridden, because the repositories depend on it.
func TestApplyDBTimeouts_ReportsFoundRows(t *testing.T) {
	cases := []struct {
		name           string
		query, acquire time.Duration
		in             string
	}{
		{"timeouts on", 10 * time.Second, 5 * time.Second, "root:@tcp(127.0.0.1:3306)/nexus_platform?parseTime=True"},
		{"timeouts off", 0, 0, "root:@tcp(127.0.0.1:3306)/nexus_platform?parseTime=True"},
		{"explicit false overridden", 0, 0, "root:@tcp(127.0.0.1:3306)/nexus_platform?parseTime=True&clientFoundRows=false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := ApplyDBTimeouts(tc.in, tc.query, tc.acquire)
			cfg, err := gomysql.ParseDSN(out)
			if err != nil {
				t.Fatalf("ParseDSN(%q): %v", out, err)
			}
			if !cfg.ClientFoundRows {
				t.Errorf("ClientFoundRows = false in %q; want true (RowsAffected must count matched rows)", out)
			}
			if !cfg.ParseTime || cfg.DBName != "nexus_platform" {
				t.Errorf("existing params lost in %q", out)
			}
		})
	}
}

// TestApplyDBTimeouts_UnparseableReturnedUnchanged verifies a DSN the driver cannot parse is
// returned verbatim (fail-safe: a bad timeout config never bricks boot).
func TestApplyDBTimeouts_UnparseableReturnedUnchanged(t *testing.T) {
	in := "this is not a dsn"
	if out := ApplyDBTimeouts(in, 10*time.Second, 5*time.Second); out != in {
		t.Errorf("unparseable DSN should be returned unchanged; got %q", out)
	}
}

// TestLoadConfig_DBTimeoutDefaults verifies the W5-E2 timeout defaults are applied when env is
// unset and that LoadConfig folds them into DBDSN (the DSN the pools open).
func TestLoadConfig_DBTimeoutDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", testDSN)
	t.Setenv("DB_QUERY_TIMEOUT_SEC", "")
	t.Setenv("DB_CONN_ACQUIRE_TIMEOUT_SEC", "")

	cfg := loadConfig(t)

	if cfg.DBQueryTimeout != time.Duration(defaultDBQueryTimeoutSec)*time.Second {
		t.Errorf("DBQueryTimeout = %v, want %ds", cfg.DBQueryTimeout, defaultDBQueryTimeoutSec)
	}
	if cfg.DBConnAcquireTimeout != time.Duration(defaultDBConnAcquireTimeoutSec)*time.Second {
		t.Errorf("DBConnAcquireTimeout = %v, want %ds", cfg.DBConnAcquireTimeout, defaultDBConnAcquireTimeoutSec)
	}
	// The DSN must carry the readTimeout so the pools inherit it.
	parsed, err := gomysql.ParseDSN(cfg.DBDSN)
	if err != nil {
		t.Fatalf("ParseDSN(cfg.DBDSN=%q): %v", cfg.DBDSN, err)
	}
	if parsed.ReadTimeout != time.Duration(defaultDBQueryTimeoutSec)*time.Second {
		t.Errorf("DBDSN ReadTimeout = %v, want %ds", parsed.ReadTimeout, defaultDBQueryTimeoutSec)
	}
	if parsed.Timeout != time.Duration(defaultDBConnAcquireTimeoutSec)*time.Second {
		t.Errorf("DBDSN Timeout = %v, want %ds", parsed.Timeout, defaultDBConnAcquireTimeoutSec)
	}
	if !parsed.ClientFoundRows {
		t.Errorf("DBDSN %q lacks clientFoundRows; both pools must count matched rows (queue #91)", cfg.DBDSN)
	}
}

// TestLoadConfig_DBTimeoutFromEnv verifies env overrides flow through into DBDSN.
func TestLoadConfig_DBTimeoutFromEnv(t *testing.T) {
	t.Setenv("DATABASE_URL", testDSN)
	t.Setenv("DB_QUERY_TIMEOUT_SEC", "3")
	t.Setenv("DB_CONN_ACQUIRE_TIMEOUT_SEC", "2")

	cfg := loadConfig(t)

	if cfg.DBQueryTimeout != 3*time.Second {
		t.Errorf("DBQueryTimeout = %v, want 3s", cfg.DBQueryTimeout)
	}
	if cfg.DBConnAcquireTimeout != 2*time.Second {
		t.Errorf("DBConnAcquireTimeout = %v, want 2s", cfg.DBConnAcquireTimeout)
	}
	if !strings.Contains(cfg.DBDSN, "readTimeout=3s") {
		t.Errorf("DBDSN = %q, want readTimeout=3s", cfg.DBDSN)
	}
}
