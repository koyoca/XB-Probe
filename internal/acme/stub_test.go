package acme

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubCA 是一个内存版的最小 ACME v2 服务端，只用于测试。
//
// 它刻意把"服务端该做的校验"都真做一遍：验 JWS 签名、检查 nonce 只能一次性、
// 核对 keyAuthorization 与账户指纹是否一致、核对 CSR 的 SAN 是否等于订单标识符。
// 这样客户端任何一处细节写错（签名输入、定长 R||S、POST-as-GET 的空 payload、
// 坐标没补前导零、挑战扩展值包装不对……）都会在这里被抓出来，
// 而不是等到真机上线才发现。
type stubCA struct {
	t      *testing.T
	server *httptest.Server

	mu         sync.Mutex
	nonces     map[string]bool
	accounts   map[string]*stubAccount // kid -> 账户
	orders     map[string]*stubOrder
	authzs     map[string]*stubAuthz
	challenges map[string]*stubChallenge
	certPEM    map[string][]byte
	seq        int

	lastContact []string // 最近一次 newAccount 提交的联系方式

	// countNewOrders 非空时，每收到一次 newOrder 就加一。
	// 用来断言"某些前置条件不满足时，根本不该向 CA 建订单"——
	// 建了又跑不完的订单会白占 CA 的速率配额。
	countNewOrders *int

	// badNonceOnce 为 true 时，下一个受保护的 POST 会被回一个 badNonce 错误，
	// 用来验证客户端"nonce 失效要重试一次"的健壮性。
	badNonceOnce bool

	// validate 由测试注入：给定挑战类型、标识符与 token，返回"CA 视角看到的应答内容"。
	// http-01 返回从 HTTP 端点抓到的 keyAuthorization；
	// tls-alpn-01 返回从挑战证书 acmeIdentifier 扩展里解出的摘要（base64url）。
	validate func(challengeType, identifier, token string) (string, error)

	caKey  *ecdsa.PrivateKey
	caCert *x509.Certificate
}

type stubAccount struct {
	kid string
	jwk jwkEC
}

type stubOrder struct {
	id          string
	identifiers []Identifier
	authzIDs    []string
	certURL     string
}

type stubAuthz struct {
	id         string
	identifier Identifier
	// wildcard 模拟真实 CA 的行为：订单里写 *.example.com，
	// 授权里的 identifier 是 example.com + wildcard=true
	//（RFC 8555 §7.1.4：标识符值不带 `*.`）。客户端必须据此算出
	// _acme-challenge.example.com，而不是 _acme-challenge.*.example.com。
	wildcard bool
	challIDs []string
	status   string // pending | valid | invalid
}

type stubChallenge struct {
	id     string
	authID string
	typ    string
	token  string
	status string
}

type jwsProtected struct {
	Alg   string `json:"alg"`
	Nonce string `json:"nonce"`
	URL   string `json:"url"`
	Kid   string `json:"kid"`
	JWK   *jwkEC `json:"jwk"`
}

type parsedJWS struct {
	hdr     jwsProtected
	payload []byte
}

func newStubCA(t *testing.T, validate func(challengeType, identifier, token string) (string, error)) *stubCA {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成桩 CA 密钥失败: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Stub ACME CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(1, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, caKey.Public(), caKey)
	if err != nil {
		t.Fatalf("自签桩 CA 失败: %v", err)
	}
	caCert, _ := x509.ParseCertificate(der)

	s := &stubCA{
		t:          t,
		nonces:     map[string]bool{},
		accounts:   map[string]*stubAccount{},
		orders:     map[string]*stubOrder{},
		authzs:     map[string]*stubAuthz{},
		challenges: map[string]*stubChallenge{},
		certPEM:    map[string][]byte{},
		validate:   validate,
		caKey:      caKey,
		caCert:     caCert,
	}
	s.server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.server.Close)
	return s
}

func (s *stubCA) url(path string) string { return s.server.URL + path }
func (s *stubCA) directoryURL() string   { return s.url("/directory") }

// ---------------------------------------------------------------------------
// 路由
// ---------------------------------------------------------------------------

func (s *stubCA) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/directory":
		s.writeJSON(w, http.StatusOK, map[string]string{
			"newNonce":   s.url("/new-nonce"),
			"newAccount": s.url("/new-account"),
			"newOrder":   s.url("/new-order"),
			"revokeCert": s.url("/revoke-cert"),
			"keyChange":  s.url("/key-change"),
		}, "")
		return
	case "/new-nonce":
		if r.Method != http.MethodHead && r.Method != http.MethodGet {
			s.problem(w, http.StatusMethodNotAllowed, "malformed", "new-nonce 只接受 HEAD")
			return
		}
		w.Header().Set("Replay-Nonce", s.newNonce())
		w.WriteHeader(http.StatusOK)
		return
	}

	if r.Method != http.MethodPost {
		s.problem(w, http.StatusMethodNotAllowed, "malformed", "ACME 端点只接受 POST")
		return
	}
	s.mu.Lock()
	injectBadNonce := s.badNonceOnce
	s.badNonceOnce = false
	s.mu.Unlock()
	if injectBadNonce {
		s.problem(w, http.StatusBadRequest, "badNonce", "nonce 已失效（测试注入）")
		return
	}
	jws, err := s.parseAndVerifyJWS(r)
	if err != nil {
		s.problem(w, http.StatusBadRequest, "malformed", err.Error())
		return
	}

	path := r.URL.Path
	switch {
	case path == "/new-account":
		s.handleNewAccount(w, jws)
	case path == "/new-order":
		s.handleNewOrder(w, jws)
	case strings.HasPrefix(path, "/authz/"):
		s.handleAuthz(w, jws, strings.TrimPrefix(path, "/authz/"))
	case strings.HasPrefix(path, "/challenge/"):
		s.handleChallenge(w, jws, strings.TrimPrefix(path, "/challenge/"))
	case strings.HasPrefix(path, "/order/"):
		rest := strings.TrimPrefix(path, "/order/")
		if id, ok := strings.CutSuffix(rest, "/finalize"); ok {
			s.handleFinalize(w, jws, id)
			return
		}
		s.handleOrder(w, jws, rest)
	case strings.HasPrefix(path, "/cert/"):
		s.handleCert(w, jws, strings.TrimPrefix(path, "/cert/"))
	default:
		s.problem(w, http.StatusNotFound, "malformed", "未知端点: "+path)
	}
}

func (s *stubCA) writeJSON(w http.ResponseWriter, status int, body any, location string) {
	w.Header().Set("Replay-Nonce", s.newNonce())
	w.Header().Set("Content-Type", "application/json")
	if location != "" {
		w.Header().Set("Location", location)
	}
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func (s *stubCA) problem(w http.ResponseWriter, status int, typ, detail string) {
	s.writeJSON(w, status, map[string]any{
		"type":   "urn:ietf:params:acme:error:" + typ,
		"detail": detail,
		"status": status,
	}, "")
}

func (s *stubCA) newNonce() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	n := fmt.Sprintf("nonce-%d", s.seq)
	s.nonces[n] = true
	return n
}

// ---------------------------------------------------------------------------
// JWS 校验
// ---------------------------------------------------------------------------

func (s *stubCA) parseAndVerifyJWS(r *http.Request) (*parsedJWS, error) {
	var raw struct {
		Protected string `json:"protected"`
		Payload   string `json:"payload"`
		Signature string `json:"signature"`
	}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("请求体不是 flattened JWS: %w", err)
	}
	protJSON, err := base64.RawURLEncoding.DecodeString(raw.Protected)
	if err != nil {
		return nil, fmt.Errorf("protected 不是 base64url: %w", err)
	}
	var hdr jwsProtected
	if err := json.Unmarshal(protJSON, &hdr); err != nil {
		return nil, fmt.Errorf("protected 不是 JSON: %w", err)
	}
	if hdr.Alg != "ES256" {
		return nil, fmt.Errorf("alg 必须是 ES256，实际 %q", hdr.Alg)
	}
	if hdr.URL != s.url(r.URL.Path) {
		return nil, fmt.Errorf("受保护头里的 url 与请求路径不一致：%q", hdr.URL)
	}
	s.mu.Lock()
	ok := s.nonces[hdr.Nonce]
	delete(s.nonces, hdr.Nonce)
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("nonce %q 无效或已被使用（nonce 必须一次性）", hdr.Nonce)
	}

	var pub *ecdsa.PublicKey
	switch {
	case hdr.JWK != nil && hdr.Kid != "":
		return nil, fmt.Errorf("protected 不能同时带 jwk 与 kid")
	case hdr.JWK != nil:
		p, err := publicFromJWK(*hdr.JWK)
		if err != nil {
			return nil, err
		}
		pub = p
	case hdr.Kid != "":
		s.mu.Lock()
		acct := s.accounts[hdr.Kid]
		s.mu.Unlock()
		if acct == nil {
			return nil, fmt.Errorf("未知账户 kid: %q", hdr.Kid)
		}
		p, err := publicFromJWK(acct.jwk)
		if err != nil {
			return nil, err
		}
		pub = p
	default:
		return nil, fmt.Errorf("protected 必须带 jwk 或 kid")
	}

	sig, err := base64.RawURLEncoding.DecodeString(raw.Signature)
	if err != nil {
		return nil, fmt.Errorf("signature 不是 base64url: %w", err)
	}
	if len(sig) != 64 {
		return nil, fmt.Errorf("ES256 签名必须是 64 字节定长，实际 %d", len(sig))
	}
	digest := sha256.Sum256([]byte(raw.Protected + "." + raw.Payload))
	if !ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return nil, fmt.Errorf("JWS 签名验签失败")
	}

	var payload []byte
	if raw.Payload != "" {
		payload, err = base64.RawURLEncoding.DecodeString(raw.Payload)
		if err != nil {
			return nil, fmt.Errorf("payload 不是 base64url: %w", err)
		}
	}
	return &parsedJWS{hdr: hdr, payload: payload}, nil
}

func publicFromJWK(k jwkEC) (*ecdsa.PublicKey, error) {
	if k.Crv != "P-256" || k.Kty != "EC" {
		return nil, fmt.Errorf("只支持 P-256/EC，收到 crv=%q kty=%q", k.Crv, k.Kty)
	}
	x, err := base64.RawURLEncoding.DecodeString(k.X)
	if err != nil {
		return nil, fmt.Errorf("JWK.x 不是 base64url: %w", err)
	}
	y, err := base64.RawURLEncoding.DecodeString(k.Y)
	if err != nil {
		return nil, fmt.Errorf("JWK.y 不是 base64url: %w", err)
	}
	if len(x) != 32 || len(y) != 32 {
		return nil, fmt.Errorf("P-256 坐标必须是 32 字节定长，实际 %d/%d（多半没补前导零）", len(x), len(y))
	}
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	if !pub.Curve.IsOnCurve(pub.X, pub.Y) {
		return nil, fmt.Errorf("JWK 坐标不在 P-256 曲线上")
	}
	return pub, nil
}

// ---------------------------------------------------------------------------
// 端点
// ---------------------------------------------------------------------------

func (s *stubCA) handleNewAccount(w http.ResponseWriter, jws *parsedJWS) {
	if jws.hdr.JWK == nil {
		s.problem(w, http.StatusBadRequest, "malformed", "newAccount 必须带 jwk")
		return
	}
	var body struct {
		TermsOfServiceAgreed bool     `json:"termsOfServiceAgreed"`
		Contact              []string `json:"contact"`
	}
	_ = json.Unmarshal(jws.payload, &body)
	if !body.TermsOfServiceAgreed {
		s.problem(w, http.StatusBadRequest, "malformed", "必须同意服务条款")
		return
	}
	s.mu.Lock()
	kid := fmt.Sprintf("%s/acct/%d", s.server.URL, len(s.accounts)+1)
	s.accounts[kid] = &stubAccount{kid: kid, jwk: *jws.hdr.JWK}
	s.lastContact = body.Contact
	s.mu.Unlock()
	s.writeJSON(w, http.StatusCreated, map[string]any{"status": "valid"}, kid)
}

func (s *stubCA) handleNewOrder(w http.ResponseWriter, jws *parsedJWS) {
	if jws.hdr.Kid == "" {
		s.problem(w, http.StatusUnauthorized, "accountDoesNotExist", "newOrder 必须带 kid")
		return
	}
	var body struct {
		Identifiers []Identifier `json:"identifiers"`
	}
	if err := json.Unmarshal(jws.payload, &body); err != nil {
		s.problem(w, http.StatusBadRequest, "malformed", "订单请求体不是 JSON")
		return
	}
	if len(body.Identifiers) == 0 {
		s.problem(w, http.StatusBadRequest, "malformed", "订单必须至少有一个标识符")
		return
	}
	s.mu.Lock()
	if s.countNewOrders != nil {
		*s.countNewOrders++
	}
	oid := fmt.Sprintf("%d", len(s.orders)+1)
	ord := &stubOrder{id: oid, identifiers: body.Identifiers}
	for i, id := range body.Identifiers {
		aid := fmt.Sprintf("%s-%d", oid, i)
		az := &stubAuthz{id: aid, identifier: id, status: "pending"}
		// 通配符：授权里的标识符去掉 `*.`，另用 wildcard 标志表达。
		if strings.HasPrefix(id.Value, "*.") {
			az.identifier = Identifier{Type: id.Type, Value: strings.TrimPrefix(id.Value, "*.")}
			az.wildcard = true
		}
		// 三种挑战都提供，与真实的 Let's Encrypt 一致；客户端选哪种由调用方决定。
		for _, ctyp := range []string{ChallengeHTTP01, ChallengeTLSALPN01, ChallengeDNS01} {
			cid := aid + "-" + ctyp
			s.challenges[cid] = &stubChallenge{
				id: cid, authID: aid, typ: ctyp,
				token: "tok-" + cid, status: "pending",
			}
			az.challIDs = append(az.challIDs, cid)
		}
		s.authzs[aid] = az
		ord.authzIDs = append(ord.authzIDs, aid)
	}
	s.orders[oid] = ord
	s.mu.Unlock()
	s.writeJSON(w, http.StatusCreated, s.orderBody(ord), s.url("/order/"+oid))
}

// 下面几个 xxxBody 都在持有 s.mu 之前先取快照，避免嵌套加锁。
func (s *stubCA) orderBody(o *stubOrder) map[string]any {
	s.mu.Lock()
	status := "pending"
	allValid := len(o.authzIDs) > 0
	for _, aid := range o.authzIDs {
		if s.authzs[aid].status != "valid" {
			allValid = false
		}
	}
	if allValid {
		status = "ready"
	}
	if o.certURL != "" {
		status = "valid"
	}
	authzURLs := make([]string, 0, len(o.authzIDs))
	for _, aid := range o.authzIDs {
		authzURLs = append(authzURLs, s.url("/authz/"+aid))
	}
	certURL := o.certURL
	ids := append([]Identifier{}, o.identifiers...)
	s.mu.Unlock()

	body := map[string]any{
		"status":         status,
		"expires":        time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		"identifiers":    ids,
		"authorizations": authzURLs,
		"finalize":       s.url("/order/" + o.id + "/finalize"),
	}
	if certURL != "" {
		body["certificate"] = certURL
	}
	return body
}

func (s *stubCA) authzBody(a *stubAuthz) map[string]any {
	s.mu.Lock()
	challs := make([]map[string]any, 0, len(a.challIDs))
	for _, cid := range a.challIDs {
		c := s.challenges[cid]
		challs = append(challs, map[string]any{
			"type": c.typ, "url": s.url("/challenge/" + c.id), "token": c.token, "status": c.status,
		})
	}
	status, identifier, wildcard := a.status, a.identifier, a.wildcard
	s.mu.Unlock()
	return map[string]any{
		"status":     status,
		"identifier": identifier,
		"wildcard":   wildcard,
		"challenges": challs,
		"expires":    time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	}
}

func (s *stubCA) handleAuthz(w http.ResponseWriter, jws *parsedJWS, id string) {
	if len(jws.payload) != 0 {
		s.problem(w, http.StatusBadRequest, "malformed", "authz 只接受 POST-as-GET（空 payload）")
		return
	}
	s.mu.Lock()
	az := s.authzs[id]
	s.mu.Unlock()
	if az == nil {
		s.problem(w, http.StatusNotFound, "malformed", "未知授权")
		return
	}
	s.writeJSON(w, http.StatusOK, s.authzBody(az), "")
}

func (s *stubCA) handleChallenge(w http.ResponseWriter, jws *parsedJWS, id string) {
	s.mu.Lock()
	c := s.challenges[id]
	s.mu.Unlock()
	if c == nil {
		s.problem(w, http.StatusNotFound, "malformed", "未知挑战")
		return
	}
	var body map[string]any
	_ = json.Unmarshal(jws.payload, &body)
	if len(body) != 0 {
		s.problem(w, http.StatusBadRequest, "malformed", "触发挑战必须提交空对象 {}")
		return
	}
	s.answerChallenge(c)
	s.mu.Lock()
	status, typ := c.status, c.typ
	s.mu.Unlock()
	s.writeJSON(w, http.StatusOK, map[string]any{
		"type": typ, "url": s.url("/challenge/" + c.id), "status": status,
	}, "")
}

func (s *stubCA) handleOrder(w http.ResponseWriter, jws *parsedJWS, id string) {
	if len(jws.payload) != 0 {
		s.problem(w, http.StatusBadRequest, "malformed", "order 只接受 POST-as-GET（空 payload）")
		return
	}
	s.mu.Lock()
	o := s.orders[id]
	s.mu.Unlock()
	if o == nil {
		s.problem(w, http.StatusNotFound, "malformed", "未知订单")
		return
	}
	s.writeJSON(w, http.StatusOK, s.orderBody(o), "")
}

func (s *stubCA) handleFinalize(w http.ResponseWriter, jws *parsedJWS, id string) {
	s.mu.Lock()
	o := s.orders[id]
	s.mu.Unlock()
	if o == nil {
		s.problem(w, http.StatusNotFound, "malformed", "未知订单")
		return
	}
	var body struct {
		CSR string `json:"csr"`
	}
	if err := json.Unmarshal(jws.payload, &body); err != nil || body.CSR == "" {
		s.problem(w, http.StatusBadRequest, "malformed", "finalize 需要 csr 字段")
		return
	}
	der, err := base64.RawURLEncoding.DecodeString(body.CSR)
	if err != nil {
		s.problem(w, http.StatusBadRequest, "malformed", "csr 不是 base64url")
		return
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		s.problem(w, http.StatusBadRequest, "badCSR", "CSR 解析失败: "+err.Error())
		return
	}
	// 真实 CA 会核对 CSR 的 SAN 与订单标识符完全一致；这里也照做，
	// 否则"客户端漏了一个域名"这类错误会从测试里漏过去。
	if err := checkCSRMatches(csr, o.identifiers); err != nil {
		s.problem(w, http.StatusBadRequest, "badCSR", err.Error())
		return
	}
	certPEM, err := s.issue(csr)
	if err != nil {
		s.problem(w, http.StatusInternalServerError, "serverInternal", err.Error())
		return
	}
	s.mu.Lock()
	certID := fmt.Sprintf("%d", len(s.certPEM)+1)
	s.certPEM[certID] = certPEM
	o.certURL = s.url("/cert/" + certID)
	s.mu.Unlock()
	s.writeJSON(w, http.StatusOK, s.orderBody(o), "")
}

func (s *stubCA) handleCert(w http.ResponseWriter, jws *parsedJWS, id string) {
	if len(jws.payload) != 0 {
		s.problem(w, http.StatusBadRequest, "malformed", "证书只能 POST-as-GET 下载")
		return
	}
	s.mu.Lock()
	raw, ok := s.certPEM[id]
	s.mu.Unlock()
	if !ok {
		s.problem(w, http.StatusNotFound, "malformed", "未知证书")
		return
	}
	w.Header().Set("Replay-Nonce", s.newNonce())
	w.Header().Set("Content-Type", "application/pem-certificate-chain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func checkCSRMatches(csr *x509.CertificateRequest, ids []Identifier) error {
	if got := len(csr.IPAddresses) + len(csr.DNSNames); got != len(ids) {
		return fmt.Errorf("CSR 的 SAN 数量(%d)与订单标识符数量(%d)不一致", got, len(ids))
	}
	for _, id := range ids {
		if id.Type == "ip" {
			found := false
			for _, ip := range csr.IPAddresses {
				if ip.String() == id.Value {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("CSR 缺少 IP SAN %s", id.Value)
			}
			continue
		}
		found := false
		for _, d := range csr.DNSNames {
			if strings.EqualFold(d, id.Value) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("CSR 缺少 DNS SAN %s", id.Value)
		}
	}
	return nil
}

func (s *stubCA) issue(csr *x509.CertificateRequest) ([]byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      csr.Subject,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(0, 0, 90),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     csr.DNSNames,
		IPAddresses:  csr.IPAddresses,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, s.caCert, csr.PublicKey, s.caKey)
	if err != nil {
		return nil, err
	}
	leaf := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.caCert.Raw})
	return append(leaf, ca...), nil
}

// answerChallenge 用注入的 validate 校验应答，并推进挑战与授权的状态。
//
// 这一步是整套测试的价值所在：客户端算出的 keyAuthorization 只要与
// 账户 JWK 指纹对不上（字段序、前导零、base64 变体任一写错都会对不上），
// 授权就会停在 invalid，Obtain 随即失败。
//
// 两种挑战的"期望值"不同，真实 CA 就是这么比对的：
//   - http-01：应答内容原样就是 keyAuthorization；
//   - tls-alpn-01：证书里带的是 SHA-256(keyAuthorization) 的摘要；
//   - dns-01：TXT 记录里带的也是 SHA-256(keyAuthorization) 的摘要
//     （与 tls-alpn-01 同一个摘要，只是载体不同）。
func (s *stubCA) answerChallenge(c *stubChallenge) {
	s.mu.Lock()
	az := s.authzs[c.authID]
	s.mu.Unlock()
	if az == nil {
		return
	}
	keyAuth := keyAuthorization(c.token, s.accountThumbprint())
	want := keyAuth
	if c.typ == ChallengeTLSALPN01 || c.typ == ChallengeDNS01 {
		sum := sha256.Sum256([]byte(keyAuth))
		want = base64.RawURLEncoding.EncodeToString(sum[:])
	}
	got, err := s.validate(c.typ, az.identifier.Value, c.token)
	ok := err == nil && got == want
	s.mu.Lock()
	defer s.mu.Unlock()
	if ok {
		c.status = "valid"
		az.status = "valid"
	} else {
		c.status = "invalid"
		az.status = "invalid"
	}
}

func (s *stubCA) accountThumbprint() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		return a.jwk.thumbprint()
	}
	return ""
}
