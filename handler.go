package influxdb

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/bmizerany/pat"
	"github.com/influxdb/influxdb/influxql"
)

// TODO: Standard response headers (see: HeaderHandler)
// TODO: Compression (see: CompressionHeaderHandler)

// TODO: Check HTTP response codes: 400, 401, 403, 409.

// getUsernameAndPassword returns the username and password encoded in
// a request. The credentials may be present as URL query params, or as
// a Basic Authentication header.
func getUsernameAndPassword(r *http.Request) (string, string, error) {
	q := r.URL.Query()
	username, password := q.Get("u"), q.Get("p")
	if username != "" && password != "" {
		return username, password, nil
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", "", nil
	}
	fields := strings.Split(auth, " ")
	if len(fields) != 2 {
		return "", "", fmt.Errorf("invalid Basic Authentication header")
	}
	bs, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return "", "", fmt.Errorf("invalid Base64 encoding")
	}
	fields = strings.Split(string(bs), ":")
	if len(fields) != 2 {
		return "", "", fmt.Errorf("invalid Basic Authentication value")
	}
	return fields[0], fields[1], nil
}

// Handler represents an HTTP handler for the InfluxDB server.
type Handler struct {
	server *Server
	mux    *pat.PatternServeMux

	// Whether endpoints require authentication.
	AuthenticationEnabled bool

	// The InfluxDB verion returned by the HTTP response header.
	Version string
}

// NewHandler returns a new instance of Handler.
func NewHandler(s *Server) *Handler {
	h := &Handler{
		server: s,
		mux:    pat.New(),
	}

	// Query serving route.
	h.mux.Get("/query", h.makeAuthenticationHandler(h.serveQuery))

	// Data-ingest route.
	h.mux.Post("/db/:db/series", h.makeAuthenticationHandler(h.serveWriteSeries))

	// Shard routes.
	h.mux.Get("/db/:db/shards", h.makeAuthenticationHandler(h.serveShards))
	h.mux.Del("/db/:db/shards/:id", h.makeAuthenticationHandler(h.serveDeleteShard))

	// Data node routes.
	h.mux.Get("/data_nodes", h.makeAuthenticationHandler(h.serveDataNodes))
	h.mux.Post("/data_nodes", h.makeAuthenticationHandler(h.serveCreateDataNode))
	h.mux.Del("/data_nodes/:id", h.makeAuthenticationHandler(h.serveDeleteDataNode))

	// Utilities
	h.mux.Get("/ping", h.makeAuthenticationHandler(h.servePing))

	return h
}

// ServeHTTP responds to HTTP request to the handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Access-Control-Allow-Origin", "*")
	w.Header().Add("Access-Control-Max-Age", "2592000")
	w.Header().Add("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE")
	w.Header().Add("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept")
	w.Header().Add("X-Influxdb-Version", h.Version)

	// If this is a CORS OPTIONS request then send back okie-dokie.
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Otherwise handle it via pat.
	h.mux.ServeHTTP(w, r)
}

// makeAuthenticationHandler takes a custom handler and returns a standard handler, ensuring that
// if user credentials are passed in, an attempt is made to authenticate that user. If authentication
// fails, an error is returned to the user.
//
// There is one exception: if there are no users in the system, authentication is not required. This
// is to facilitate bootstrapping of a system with authentication enabled.
func (h *Handler) makeAuthenticationHandler(fn func(http.ResponseWriter, *http.Request, *User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var user *User
		if h.AuthenticationEnabled && len(h.server.Users()) > 0 {
			username, password, err := getUsernameAndPassword(r)
			if err != nil {
				h.error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			if username == "" {
				h.error(w, "username required", http.StatusUnauthorized)
				return
			}

			user, err = h.server.Authenticate(username, password)
			if err != nil {
				h.error(w, err.Error(), http.StatusUnauthorized)
				return
			}
		}
		fn(w, r, user)
	}
}

// serveQuery parses an incoming query and returns the results.
func (h *Handler) serveQuery(w http.ResponseWriter, r *http.Request, u *User) {
	q := r.URL.Query()
	p := influxql.NewParser(strings.NewReader(q.Get("q")))

	// Parse query from query string.
	query, err := p.ParseQuery()
	if err != nil {
		h.error(w, "error parsing query: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Bootstrapping a secured cluster involves a special authentication policy.
	// Until an admin-level user exists on the cluster one, and only one operation
	// is permitted without authentication -- the creation of the first user.
	// This user is also automatically granted admin-level access.
	if h.AuthenticationEnabled && len(h.server.Users()) > 0 {
		if len(query.Statements) != 1 {
			_, ok := query.Statements[0].(*influxql.CreateUserStatement)
			if !ok {
				h.error(w, "initial admin user does not exist", http.StatusUnauthorized)
				return
			}
		}
	}

	for _, s := range query.Statements {
		switch c := s.(type) {
		case *influxql.CreateDatabaseStatement:
			err = h.server.CreateDatabase(c.Name)
		case *influxql.DropDatabaseStatement:
			err = h.server.DeleteDatabase(c.Name)

		case *influxql.CreateUserStatement:
			err = h.server.CreateUser(c.Name, c.Password, !h.server.AdminUserExists()) // XXX This is obviously broken.
		case *influxql.DropUserStatement:
			err = h.server.DeleteUser(c.Name)

		case *influxql.SelectStatement:
			continue
		case *influxql.DropSeriesStatement:
			continue
		case *influxql.ListSeriesStatement:
			continue
		case *influxql.ListMeasurementsStatement:
			continue
		case *influxql.ListTagKeysStatement:
			continue
		case *influxql.ListTagValuesStatement:
			continue
		case *influxql.ListFieldKeysStatement:
			continue
		case *influxql.ListFieldValuesStatement:
			continue

		case *influxql.GrantStatement:
			continue
		case *influxql.RevokeStatement:
			continue

		case *influxql.CreateRetentionPolicyStatement:
			rp := NewRetentionPolicy(c.Name)
			rp.Duration = c.Duration
			rp.ReplicaN = uint32(c.Replication)
			err = h.server.CreateRetentionPolicy(c.DB, rp)
		case *influxql.AlterRetentionPolicyStatement:
			rp := NewRetentionPolicy(c.Name)
			rp.Duration = *c.Duration // Why is this a pointer, and the next?
			rp.ReplicaN = uint32(*c.Replication)
			err = h.server.UpdateRetentionPolicy(c.DB, c.Name, rp)

		case *influxql.CreateContinuousQueryStatement:
			continue
		case *influxql.DropContinuousQueryStatement:
			continue
		case *influxql.ListContinuousQueriesStatement:
			continue
		}

		if err != nil {
			h.error(w, "error executing query: "+err.Error(), http.StatusBadRequest)
		}
	}
}

// serveWriteSeries receives incoming series data and writes it to the database.
func (h *Handler) serveWriteSeries(w http.ResponseWriter, r *http.Request, u *User) {
	// TODO: Authentication.

	/* TEMPORARILY REMOVED FOR PROTOBUFS.
	// Retrieve database from server.
	db := h.server.Database(r.URL.Query().Get(":db"))
	if db == nil {
		h.error(w, ErrDatabaseNotFound.Error(), http.StatusNotFound)
		return
	}

	// Parse time precision from query parameters.
	precision, err := parseTimePrecision(r.URL.Query().Get("time_precision"))
	if err != nil {
		h.error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Setup HTTP request reader. Wrap in a gzip reader if encoding set in header.
	reader := r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		if reader, err = gzip.NewReader(r.Body); err != nil {
			h.error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Decode series from reader.
	ss := []*serializedSeries{}
	dec := json.NewDecoder(reader)
	dec.UseNumber()
	if err := dec.Decode(&ss); err != nil {
		h.error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Convert the wire format to the internal representation of the time series.
	series, err := serializedSeriesSlice(ss).series(precision)
	if err != nil {
		h.error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Write series data to the database.
	// TODO: Allow multiple series written to DB at once.
	for _, s := range series {
		if err := db.WriteSeries(s); err != nil {
			h.error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	*/
}

// servePing returns a simple response to let the client know the server is running.
func (h *Handler) servePing(w http.ResponseWriter, r *http.Request, u *User) {}

// serveShards returns a list of shards.
func (h *Handler) serveShards(w http.ResponseWriter, r *http.Request, u *User) {
	q := r.URL.Query()

	// Retrieves shards for the database.
	shards, err := h.server.Shards(q.Get(":db"))
	if err == ErrDatabaseNotFound {
		h.error(w, err.Error(), http.StatusNotFound)
		return
	} else if err != nil {
		h.error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Write data to the response.
	w.Header().Add("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(shards)
}

// serveDeleteShard removes an existing shard.
func (h *Handler) serveDeleteShard(w http.ResponseWriter, r *http.Request, u *User) {}

// serveRetentionPolicies returns a list of retention policys.
func (h *Handler) serveRetentionPolicies(w http.ResponseWriter, r *http.Request, u *User) {
	// Retrieve policies by database.
	policies, err := h.server.RetentionPolicies(r.URL.Query().Get(":db"))
	if err == ErrDatabaseNotFound {
		h.error(w, err.Error(), http.StatusNotFound)
		return
	} else if err != nil {
		h.error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Write data to response body.
	w.Header().Add("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(policies)
}

// serveCreateRetentionPolicy creates a new retention policy.
func (h *Handler) serveCreateRetentionPolicy(w http.ResponseWriter, r *http.Request, u *User) {
	// Decode the policy from the body.
	var policy RetentionPolicy
	if err := json.NewDecoder(r.Body).Decode(&policy); err != nil {
		h.error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Create the retention policy.
	if err := h.server.CreateRetentionPolicy(r.URL.Query().Get(":db"), &policy); err == ErrDatabaseNotFound {
		h.error(w, err.Error(), http.StatusNotFound)
		return
	} else if err == ErrRetentionPolicyExists {
		h.error(w, err.Error(), http.StatusConflict)
		return
	} else if err != nil {
		h.error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// serveUpdateRetentionPolicy updates an existing retention policy.
func (h *Handler) serveUpdateRetentionPolicy(w http.ResponseWriter, r *http.Request, u *User) {
	q := r.URL.Query()
	db, name := q.Get(":db"), q.Get(":name")

	// Decode the new policy values from the body.
	var policy RetentionPolicy
	if err := json.NewDecoder(r.Body).Decode(&policy); err != nil {
		h.error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Update the retention policy.
	if err := h.server.UpdateRetentionPolicy(db, name, &policy); err == ErrDatabaseNotFound || err == ErrRetentionPolicyNotFound {
		h.error(w, err.Error(), http.StatusNotFound)
		return
	} else if err != nil {
		h.error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serveDeleteRetentionPolicy removes an existing retention policy.
func (h *Handler) serveDeleteRetentionPolicy(w http.ResponseWriter, r *http.Request, u *User) {
	q := r.URL.Query()
	db, name := q.Get(":db"), q.Get(":name")

	// Delete the retention policy.
	if err := h.server.DeleteRetentionPolicy(db, name); err == ErrDatabaseNotFound || err == ErrRetentionPolicyNotFound {
		h.error(w, err.Error(), http.StatusNotFound)
		return
	} else if err != nil {
		h.error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serveDataNodes returns a list of all data nodes in the cluster.
func (h *Handler) serveDataNodes(w http.ResponseWriter, r *http.Request, u *User) {
	// Generate a list of objects for encoding to the API.
	a := make([]*dataNodeJSON, 0)
	for _, n := range h.server.DataNodes() {
		a = append(a, &dataNodeJSON{
			ID:  n.ID,
			URL: n.URL.String(),
		})
	}

	w.Header().Add("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(a)
}

// serveCreateDataNode creates a new data node in the cluster.
func (h *Handler) serveCreateDataNode(w http.ResponseWriter, r *http.Request, u *User) {
	// Read in data node from request body.
	var n dataNodeJSON
	if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
		h.error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Parse the URL.
	url, err := url.Parse(n.URL)
	if err != nil {
		h.error(w, "invalid data node url", http.StatusBadRequest)
		return
	}

	// Create the data node.
	if err := h.server.CreateDataNode(url); err == ErrDataNodeExists {
		h.error(w, err.Error(), http.StatusConflict)
		return
	} else if err != nil {
		h.error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Write new node back to client.
	node := h.server.DataNodeByURL(url)
	w.WriteHeader(http.StatusCreated)
	w.Header().Add("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(&dataNodeJSON{ID: node.ID, URL: node.URL.String()})
}

// serveDeleteDataNode removes an existing node.
func (h *Handler) serveDeleteDataNode(w http.ResponseWriter, r *http.Request, u *User) {
	// Parse node id.
	nodeID, err := strconv.ParseUint(r.URL.Query().Get(":id"), 10, 64)
	if err != nil {
		h.error(w, "invalid node id", http.StatusBadRequest)
		return
	}

	// Delete the node.
	if err := h.server.DeleteDataNode(nodeID); err == ErrDataNodeNotFound {
		h.error(w, err.Error(), http.StatusNotFound)
		return
	} else if err != nil {
		h.error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

type dataNodeJSON struct {
	ID  uint64 `json:"id"`
	URL string `json:"url"`
}

// error returns an error to the client in a standard format.
func (h *Handler) error(w http.ResponseWriter, error string, code int) {
	// TODO: Return error as JSON.
	http.Error(w, error, code)
}
