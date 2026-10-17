package audit

/*
 * The kinds of audit event, as listed in docs/design/data-model.md §Ingest.
 * Event.Kind stays a plain string so it decodes straight from the request body;
 * these exist so emitters and the store agree on the spelling.
 */
const (
	KindConnect    = "connect"
	KindDisconnect = "disconnect"
	KindStatement  = "statement"
	KindAuthFail   = "auth_fail"
)

/*
 * The values Connection.AuthResult can take, per docs/design/data-model.md.
 * Constants for the same reason the kinds are: so emitters and the store agree
 * on the spelling.
 */
const (
	AuthOK   = "ok"
	AuthFail = "fail"
)

/*
 * Event represents the incoming JSON payload sent from the proxy.
 * Used to format the incoming JSON content for decoding, by the content
 * we mean when someone makes a POST request to POST /api/events endpoint
 */
type Event struct {
	ConnID       string `json:"conn_id"`
	Kind         string `json:"kind"`
	At           string `json:"at"`
	User         string `json:"user,omitempty"`
	ClientAddr   string `json:"client_addr,omitempty"`
	ProxyPort    int    `json:"proxy_port,omitempty"`
	Upstream     string `json:"upstream,omitempty"`
	UpstreamAddr string `json:"upstream_addr,omitempty"`
	AuthResult   string `json:"auth_result,omitempty"`
	DB           string `json:"db,omitempty"`
	SQL          string `json:"sql,omitempty"`
	StmtType     string `json:"stmt_type,omitempty"`
	Err          string `json:"err,omitempty"`
}

/*
 * Sink receives audit events. It is the proxy's only outward dependency, so the
 * relay does not know whether events are logged, buffered or posted to the API.
 */
type Sink interface {
	Emit(Event)
}

/*
 * Connection represents a single row returned from the connections database table.
 * Used to format the JSON response for the GET /api/connections endpoint.
 */
type Connection struct {
	ConnID       string `json:"conn_id"`
	DBUser       string `json:"db_user"`
	ClientAddr   string `json:"client_addr"`
	ProxyPort    int    `json:"proxy_port"`
	Upstream     string `json:"upstream"`
	UpstreamAddr string `json:"upstream_addr"`
	AuthResult   string `json:"auth_result"`
	// TsConnect is nullable: a disconnect or auth_fail event can create the row
	// before its connect event arrives, and there is no connect time to invent.
	TsConnect      *string `json:"ts_connect"`
	TsDisconnect   *string `json:"ts_disconnect"`
	StatementCount int     `json:"statement_count"`
	// AuthFailReason is the auth_fail event's Err text — the "reason" half of
	// "a failed login produces an auth_fail event with the attempted username
	// and reason" — set only for a row created or updated by a rejected login,
	// nil for every other connection.
	AuthFailReason *string `json:"auth_fail_reason,omitempty"`
}

/*
 * Statement represents a single queried statement returned to the frontend.
 * This ensures the GET /api/statements response matches the exact shape
 * expected by the UI, rather than leaking the internal ingest format.
 */
type Statement struct {
	ID       int     `json:"id"`
	ConnID   string  `json:"conn_id"`
	Ts       string  `json:"ts"`
	DBUser   string  `json:"db_user"`
	Upstream string  `json:"upstream"`
	DB       *string `json:"db"`
	SQL      string  `json:"sql"`
	StmtType string  `json:"stmt_type"`
	Error    *string `json:"error"`
}

/*
 * Server represents a configured proxy server instance.
 * Mirrors the config file (read-only for the MVP).
 */
type Server struct {
	TLS          string `json:"tls"`
	TLSCAFile    string `json:"tls_ca_file"`
	ID           int    `json:"id"`
	Name         string `json:"name"`
	ListenPort   int    `json:"listen_port"`
	UpstreamHost string `json:"upstream_host"`
	UpstreamPort int    `json:"upstream_port"`
	UpstreamUser string `json:"upstream_user"`
	UpstreamPass string `json:"upstream_pass"`
	Enabled      bool   `json:"enabled"`
}

/*
 * LoginRequest represents the expected JSON payload for a user login attempt,
 * containing the user's plain-text credentials.
 */
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

/*
 * LoginResponse represents the JSON response returned upon successful authentication,
 * providing the generated session bearer token to the client, plus the admin it
 * belongs to so the back-office can tell the signed-in admin from the others.
 */
type LoginResponse struct {
	Username string `json:"username"`
}
