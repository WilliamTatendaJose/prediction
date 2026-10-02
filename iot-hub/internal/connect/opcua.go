package connect

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gopcua/opcua"
	"github.com/gopcua/opcua/ua"
)

// OPCUAConfig is one OPC UA server (e.g. a Siemens S7-1500, a Kepware or
// Ignition gateway). Its nodes become fields of one hub sensor.
type OPCUAConfig struct {
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"` // opc.tcp://host:4840
	Sensor   string `json:"sensor"`
	// Interval is the subscription publishing/sampling interval (default 1s).
	Interval Duration `json:"interval"`
	// Heartbeat reads every node at least this often (default 60s), so an
	// unchanged tag never looks stale and a server whose deadband misses
	// slow drifts is corrected. Sample reads every node every Interval
	// instead (evenly spaced samples: unbiased averages, more rows).
	Heartbeat Duration `json:"heartbeat"`
	Sample    bool     `json:"sample"`
	// Security: None (default) or Basic256Sha256 with Sign / SignAndEncrypt.
	SecurityPolicy string `json:"securityPolicy"`
	SecurityMode   string `json:"securityMode"`
	// Client certificate for secure policies. If empty, one is generated in
	// CertDir on first start; trust it on the server.
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
	CertDir  string `json:"-"`
	Username string `json:"username"`
	Password string `json:"password"`
	Nodes    []Node `json:"nodes"`
}

type Node struct {
	Field  string `json:"field"`
	NodeID string `json:"nodeId"` // ns=3;s="DB1"."Temp"  or  ns=2;i=1001
	// Scale/Offset apply to numbers. Deadband (absolute) is enforced by the
	// server, so suppressed changes never cross the network.
	Scale    *float64 `json:"scale"`
	Offset   float64  `json:"offset"`
	Deadband float64  `json:"deadband"`
	Unit     string   `json:"unit"`
	Label    string   `json:"label"`
}

func (c *OPCUAConfig) validate() error {
	if c.Name == "" || c.Endpoint == "" {
		return fmt.Errorf("%w: name and endpoint are required", errConfig)
	}
	if !fieldRe.MatchString(c.Sensor) {
		return fmt.Errorf("%w: bad sensor id %q", errConfig, c.Sensor)
	}
	switch c.SecurityPolicy {
	case "", "None", "Basic256Sha256":
	default:
		return fmt.Errorf("%w: securityPolicy must be None or Basic256Sha256", errConfig)
	}
	switch c.SecurityMode {
	case "", "None", "Sign", "SignAndEncrypt":
	default:
		return fmt.Errorf("%w: securityMode must be None, Sign or SignAndEncrypt", errConfig)
	}
	if len(c.Nodes) == 0 {
		return fmt.Errorf("%w: no nodes", errConfig)
	}
	seen := map[string]bool{}
	for _, n := range c.Nodes {
		if !fieldRe.MatchString(n.Field) || seen[n.Field] {
			return fmt.Errorf("%w: bad or duplicate field %q", errConfig, n.Field)
		}
		seen[n.Field] = true
		if n.NodeID == "" { // ParseNodeID("") yields i=0, a real but wrong node
			return fmt.Errorf("%w: %s: nodeId is required", errConfig, n.Field)
		}
		if _, err := ua.ParseNodeID(n.NodeID); err != nil {
			return fmt.Errorf("%w: %s: node id %q: %v", errConfig, n.Field, n.NodeID, err)
		}
	}
	return nil
}

type opcuaConn struct {
	cfg  OPCUAConfig
	t    Target
	st   status
	down bool
}

func newOPCUA(cfg OPCUAConfig, t Target) *opcuaConn {
	c := &opcuaConn{cfg: cfg, t: t}
	c.st.s = Status{Name: cfg.Name, Kind: "opcua", Endpoint: cfg.Endpoint}
	return c
}

func (c *opcuaConn) Status() Status { return c.st.get() }

func (c *opcuaConn) Run(ctx context.Context) {
	if c.t.Define != nil {
		f := map[string]FieldInfo{}
		for _, n := range c.cfg.Nodes {
			f[n.Field] = FieldInfo{Unit: n.Unit, Label: n.Label}
		}
		c.t.Define(c.cfg.Sensor, f)
	}
	var bo backoff
	for ctx.Err() == nil {
		err := c.session(ctx, &bo)
		if ctx.Err() != nil {
			return
		}
		c.st.fail(err)
		if !c.down && c.t.Logf != nil {
			c.t.Logf("opcua %s: %v (retrying with backoff)", c.cfg.Name, err)
		}
		c.down = true
		if !sleep(ctx, bo.next()) {
			return
		}
	}
}

// session connects, subscribes and streams until the connection fails.
func (c *opcuaConn) session(ctx context.Context, bo *backoff) error {
	interval := c.cfg.Interval.or(time.Second)
	opts, err := c.options(ctx)
	if err != nil {
		return err
	}
	cl, err := opcua.NewClient(c.cfg.Endpoint, opts...)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = cl.Connect(cctx)
	cancel()
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer cl.Close(context.Background())

	notify := make(chan *opcua.PublishNotificationData, 16)
	sub, err := cl.Subscribe(ctx, &opcua.SubscriptionParameters{Interval: interval}, notify)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	defer sub.Cancel(context.Background())

	reqs := make([]*ua.MonitoredItemCreateRequest, len(c.cfg.Nodes))
	for i, n := range c.cfg.Nodes {
		id, _ := ua.ParseNodeID(n.NodeID) // validated at load
		req := opcua.NewMonitoredItemCreateRequestWithDefaults(id, ua.AttributeIDValue, uint32(i))
		req.RequestedParameters.SamplingInterval = float64(interval.Milliseconds())
		if n.Deadband > 0 {
			req.RequestedParameters.Filter = ua.NewExtensionObject(&ua.DataChangeFilter{
				Trigger: ua.DataChangeTriggerStatusValue, DeadbandType: uint32(ua.DeadbandTypeAbsolute), DeadbandValue: n.Deadband,
			})
		}
		reqs[i] = req
	}
	res, err := sub.Monitor(ctx, ua.TimestampsToReturnBoth, reqs...)
	if err != nil {
		return fmt.Errorf("monitor: %w", err)
	}
	for i, r := range res.Results {
		if r.StatusCode != ua.StatusOK && c.t.Logf != nil {
			c.t.Logf("opcua %s: node %s (%s) not monitored: %v", c.cfg.Name, c.cfg.Nodes[i].NodeID, c.cfg.Nodes[i].Field, r.StatusCode)
		}
	}
	c.st.ok(0)
	bo.reset()
	if c.down && c.t.Logf != nil {
		c.t.Logf("opcua %s: connected", c.cfg.Name)
	}
	c.down = false

	// Subscriptions report changes; reads fill in the rest. A read is the
	// truth at read time, so it is stamped with the current time.
	ids := make([]*ua.ReadValueID, len(c.cfg.Nodes))
	for i, n := range c.cfg.Nodes {
		id, _ := ua.ParseNodeID(n.NodeID)
		ids[i] = &ua.ReadValueID{NodeID: id, AttributeID: ua.AttributeIDValue}
	}
	sentAt := map[string]time.Time{}
	heartbeat := c.cfg.Heartbeat.or(time.Minute)
	tick := time.NewTicker(interval)
	defer tick.Stop()
	health := time.NewTicker(5 * time.Second)
	defer health.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-health.C:
			if cl.State() != opcua.Connected {
				return errors.New("connection lost")
			}
		case now := <-tick.C:
			due := c.cfg.Sample
			for _, n := range c.cfg.Nodes {
				due = due || now.Sub(sentAt[n.Field]) >= heartbeat
			}
			if !due {
				continue
			}
			rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			resp, err := cl.Read(rctx, &ua.ReadRequest{NodesToRead: ids, TimestampsToReturn: ua.TimestampsToReturnBoth})
			cancel()
			if err != nil {
				return fmt.Errorf("read: %w", err)
			}
			out := map[string]any{}
			for i, dv := range resp.Results {
				if i >= len(c.cfg.Nodes) || dv == nil || dv.Status != ua.StatusOK {
					continue
				}
				n := c.cfg.Nodes[i]
				if !c.cfg.Sample && now.Sub(sentAt[n.Field]) < heartbeat {
					continue
				}
				if v := convert(n, dv.Value); v != nil {
					out[n.Field] = v
					sentAt[n.Field] = now
				}
			}
			c.st.ok(1)
			if len(out) > 0 {
				c.st.counted(len(out), 0)
				c.t.Ingest(c.cfg.Sensor, now.UnixMilli(), out)
			}
		case n := <-notify:
			if n.Error != nil {
				return fmt.Errorf("publish: %w", n.Error)
			}
			dc, ok := n.Value.(*ua.DataChangeNotification)
			if !ok || c.cfg.Sample {
				continue // sample mode records reads only, for even spacing
			}
			byTS := map[int64]map[string]any{}
			bad := 0
			for _, item := range dc.MonitoredItems {
				if int(item.ClientHandle) >= len(c.cfg.Nodes) || item.Value == nil {
					continue
				}
				node := c.cfg.Nodes[item.ClientHandle]
				if item.Value.Status != ua.StatusOK {
					bad++ // bad/uncertain quality: no reading rather than a wrong one
					continue
				}
				v := convert(node, item.Value.Value)
				if v == nil {
					continue
				}
				ts := item.Value.SourceTimestamp
				if ts.IsZero() {
					ts = item.Value.ServerTimestamp
				}
				if ts.IsZero() {
					ts = time.Now()
				}
				ms := ts.UnixMilli()
				if byTS[ms] == nil {
					byTS[ms] = map[string]any{}
				}
				byTS[ms][node.Field] = v
				sentAt[node.Field] = time.Now()
			}
			c.st.ok(len(dc.MonitoredItems))
			// One notification can carry several sample times: ingest them in
			// order so the live buffers and the detector see time move forward.
			tss := make([]int64, 0, len(byTS))
			for ms := range byTS {
				tss = append(tss, ms)
			}
			sort.Slice(tss, func(i, j int) bool { return tss[i] < tss[j] })
			for _, ms := range tss {
				c.st.counted(len(byTS[ms]), 0)
				c.t.Ingest(c.cfg.Sensor, ms, byTS[ms])
			}
			if bad > 0 {
				c.st.mu.Lock()
				c.st.s.LastError = fmt.Sprintf("%d value(s) with bad quality", bad)
				c.st.mu.Unlock()
			}
		}
	}
}

func convert(n Node, v *ua.Variant) any {
	if v == nil {
		return nil
	}
	var f float64
	switch x := v.Value().(type) {
	case bool:
		return x
	case string:
		return x
	case float64:
		f = x
	case float32:
		f = float64(x)
	case int8:
		f = float64(x)
	case int16:
		f = float64(x)
	case int32:
		f = float64(x)
	case int64:
		f = float64(x)
	case uint8:
		f = float64(x)
	case uint16:
		f = float64(x)
	case uint32:
		f = float64(x)
	case uint64:
		f = float64(x)
	default:
		return nil // arrays, structures, dates: not a single measurement
	}
	if n.Scale != nil {
		f *= *n.Scale
	}
	f += n.Offset
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return f
}

func (c *opcuaConn) options(ctx context.Context) ([]opcua.Option, error) {
	policy, mode := c.cfg.SecurityPolicy, c.cfg.SecurityMode
	if policy == "" {
		policy = "None"
	}
	if mode == "" {
		mode = "None"
		if policy != "None" {
			mode = "SignAndEncrypt"
		}
	}
	gctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	eps, err := opcua.GetEndpoints(gctx, c.cfg.Endpoint)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("get endpoints: %w", err)
	}
	ep, err := opcua.SelectEndpoint(eps, policy, ua.MessageSecurityModeFromString(mode))
	if err != nil {
		return nil, fmt.Errorf("no endpoint with %s/%s: %w", policy, mode, err)
	}
	// Servers often advertise an internal hostname; keep the address we were given.
	ep.EndpointURL = c.cfg.Endpoint

	opts := []opcua.Option{
		opcua.SecurityPolicy(policy), opcua.SecurityModeString(mode),
		opcua.AutoReconnect(false), // reconnects are handled here, with backoff and status
		opcua.RequestTimeout(10 * time.Second), opcua.SessionTimeout(time.Hour),
	}
	if policy != "None" {
		cert, key, err := c.clientCert()
		if err != nil {
			return nil, err
		}
		opts = append(opts, opcua.Certificate(cert), opcua.PrivateKey(key), opcua.ApplicationURI(appURI(c.cfg.Name)))
	}
	if c.cfg.Username != "" {
		opts = append(opts, opcua.AuthUsername(c.cfg.Username, c.cfg.Password), opcua.SecurityFromEndpoint(ep, ua.UserTokenTypeUserName))
	} else {
		opts = append(opts, opcua.AuthAnonymous(), opcua.SecurityFromEndpoint(ep, ua.UserTokenTypeAnonymous))
	}
	return opts, nil
}

func appURI(name string) string { return "urn:iothub:client:" + url.PathEscape(name) }

// clientCert loads the configured certificate or creates a self-signed one
// in CertDir (RSA 2048, as Basic256Sha256 requires). The server must be
// told to trust it once (most PLCs list rejected certs for approval).
func (c *opcuaConn) clientCert() ([]byte, *rsa.PrivateKey, error) {
	certFile, keyFile := c.cfg.CertFile, c.cfg.KeyFile
	if certFile == "" {
		dir := c.cfg.CertDir
		if dir == "" {
			dir = "data"
		}
		certFile = filepath.Join(dir, "opcua-"+c.cfg.Name+".crt")
		keyFile = filepath.Join(dir, "opcua-"+c.cfg.Name+".key")
		if _, err := os.Stat(certFile); errors.Is(err, os.ErrNotExist) {
			if err := writeClientCert(certFile, keyFile, c.cfg.Name); err != nil {
				return nil, nil, err
			}
			if c.t.Logf != nil {
				c.t.Logf("opcua %s: created client certificate %s; trust it on the server", c.cfg.Name, certFile)
			}
		}
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("client certificate: %w", err)
	}
	key, ok := pair.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		return nil, nil, errors.New("client key must be RSA for OPC UA")
	}
	return pair.Certificate[0], key, nil
}

func writeClientCert(certFile, keyFile, name string) error {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	u, _ := url.Parse(appURI(name))
	host, _ := os.Hostname()
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "iothub " + name, Organization: []string{"IoT Hub"}},
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageDataEncipherment |
			x509.KeyUsageContentCommitment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		URIs:                  []*url.URL{u},
		DNSNames:              []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(certFile), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	return os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600)
}
