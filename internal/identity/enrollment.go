package identity

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

const maxEnrollmentMessage = 64 * 1024

const EnrollmentDraftVersion = 1

// EnrollmentDraft persists the client-generated key before the one-time token
// is sent. If the response or final profile write is lost, retrying the same
// draft is idempotent and cannot enroll a different key.
type EnrollmentDraft struct {
	Version          int        `json:"version"`
	Invitation       Invitation `json:"invitation"`
	DeviceName       string     `json:"device_name"`
	DevicePrivateKey string     `json:"device_private_key_pem"`
}

func NewEnrollmentDraft(invitation Invitation, deviceName string) (EnrollmentDraft, error) {
	if err := invitation.Validate(time.Now()); err != nil {
		return EnrollmentDraft{}, err
	}
	deviceName = strings.TrimSpace(deviceName)
	if deviceName == "" || len(deviceName) > 128 {
		return EnrollmentDraft{}, errors.New("device name must contain 1-128 characters")
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return EnrollmentDraft{}, fmt.Errorf("generate device identity: %w", err)
	}
	return EnrollmentDraft{
		Version: EnrollmentDraftVersion, Invitation: invitation, DeviceName: deviceName,
		DevicePrivateKey: encodeProfilePrivateKey(privateKey),
	}, nil
}

func (d EnrollmentDraft) privateKey() (ed25519.PrivateKey, error) {
	if d.Version != EnrollmentDraftVersion {
		return nil, errors.New("unsupported enrollment draft version")
	}
	if err := d.Invitation.validateEnvelope(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(d.DeviceName) == "" || len(d.DeviceName) > 128 {
		return nil, errors.New("invalid enrollment draft device name")
	}
	block, rest := pem.Decode([]byte(d.DevicePrivateKey))
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("invalid enrollment draft private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("invalid enrollment draft private key")
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("enrollment draft key is not Ed25519")
	}
	return key, nil
}

func (d EnrollmentDraft) Save(path string) error {
	if _, err := d.privateKey(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}

func LoadEnrollmentDraft(path string) (EnrollmentDraft, error) {
	info, err := os.Stat(path)
	if err != nil {
		return EnrollmentDraft{}, err
	}
	if err := checkPrivatePermissions(info); err != nil {
		return EnrollmentDraft{}, fmt.Errorf("enrollment draft: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return EnrollmentDraft{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var draft EnrollmentDraft
	if err := decoder.Decode(&draft); err != nil {
		return EnrollmentDraft{}, fmt.Errorf("decode enrollment draft: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return EnrollmentDraft{}, errors.New("enrollment draft contains trailing data")
	}
	if _, err := draft.privateKey(); err != nil {
		return EnrollmentDraft{}, err
	}
	return draft, nil
}

type enrollmentRequest struct {
	Version    int    `json:"version"`
	Token      string `json:"token"`
	DeviceName string `json:"device_name"`
	PublicKey  string `json:"public_key"`
}

type enrollmentResponse struct {
	Version           int    `json:"version"`
	Error             string `json:"error,omitempty"`
	ProviderName      string `json:"provider_name,omitempty"`
	ProviderID        string `json:"provider_id,omitempty"`
	GatewayID         string `json:"gateway_id,omitempty"`
	Endpoint          string `json:"endpoint,omitempty"`
	RootPin           string `json:"root_pin,omitempty"`
	RootCertificate   string `json:"root_certificate_pem,omitempty"`
	AccountID         string `json:"account_id,omitempty"`
	DeviceID          string `json:"device_id,omitempty"`
	DeviceCertificate string `json:"device_certificate_pem,omitempty"`
	IssuedAt          string `json:"issued_at,omitempty"`
}

type renewalRequest struct {
	Version int `json:"version"`
}

type EnrollmentService struct {
	Provider *Provider
}

func (s EnrollmentService) Serve(conn io.ReadWriter) error {
	if s.Provider == nil || s.Provider.Store == nil {
		return errors.New("enrollment service is not configured")
	}
	requestBytes, err := readEnrollmentMessage(conn)
	if err != nil {
		return err
	}
	var request enrollmentRequest
	if err := decodeStrictJSON(requestBytes, &request); err != nil {
		return s.writeError(conn, "invalid enrollment request")
	}
	if request.Version != InvitationVersion {
		return s.writeError(conn, "unsupported enrollment version")
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(request.PublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return s.writeError(conn, "invalid device public key")
	}
	now := time.Now()
	account, device, certificate, err := s.Provider.Store.EnrollDevice(request.Token, request.DeviceName, ed25519.PublicKey(publicKey), now,
		func(account Account, device Device) ([]byte, error) {
			return s.Provider.IssueDevice(account.ID, device.ID, ed25519.PublicKey(publicKey), now)
		})
	if err != nil {
		return s.writeError(conn, "invitation is invalid, expired, already used, or unavailable")
	}
	response := enrollmentResponse{
		Version: ProfileVersion, ProviderName: s.Provider.Metadata.Name,
		ProviderID: s.Provider.Metadata.ProviderID, GatewayID: s.Provider.Metadata.GatewayID,
		Endpoint: s.Provider.Metadata.Endpoint, RootPin: s.Provider.Metadata.RootPin,
		RootCertificate: string(encodeCertificate(s.Provider.RootCert)),
		AccountID:       account.ID, DeviceID: device.ID,
		DeviceCertificate: string(certificate), IssuedAt: now.UTC().Format(time.RFC3339),
	}
	return writeEnrollmentJSON(conn, response)
}

func (s EnrollmentService) writeError(conn io.Writer, message string) error {
	if err := writeEnrollmentJSON(conn, enrollmentResponse{Version: ProfileVersion, Error: message}); err != nil {
		return err
	}
	return errors.New(message)
}

// Renew reissues a short-lived certificate for the same authorized device
// key and identity. The caller's principal comes from mutual TLS; request data
// cannot select another account or device.
func (s EnrollmentService) Renew(conn io.ReadWriter, principal Principal) error {
	if s.Provider == nil || s.Provider.Store == nil {
		return errors.New("renewal service is not configured")
	}
	requestBytes, err := readEnrollmentMessage(conn)
	if err != nil {
		return err
	}
	var request renewalRequest
	if err := decodeStrictJSON(requestBytes, &request); err != nil || request.Version != ProfileVersion {
		return s.writeError(conn, "invalid renewal request")
	}
	if principal.ProviderID != s.Provider.Metadata.ProviderID {
		return s.writeError(conn, "device is not authorized")
	}
	if _, err := s.Provider.Store.Authorize(principal, time.Now()); err != nil {
		return s.writeError(conn, "device is not authorized")
	}
	now := time.Now()
	certificate, err := s.Provider.IssueDevice(principal.AccountID, principal.DeviceID, principal.PublicKey, now)
	if err != nil {
		return s.writeError(conn, "unable to renew device identity")
	}
	return writeEnrollmentJSON(conn, enrollmentResponse{
		Version: ProfileVersion, ProviderID: s.Provider.Metadata.ProviderID,
		GatewayID: s.Provider.Metadata.GatewayID, RootPin: s.Provider.Metadata.RootPin,
		AccountID: principal.AccountID, DeviceID: principal.DeviceID,
		DeviceCertificate: string(certificate), IssuedAt: now.UTC().Format(time.RFC3339),
	})
}

// RenewProfile rotates the certificate without rotating or exporting the
// device's private key. A revoked or expired certificate cannot renew and
// requires a fresh one-time invitation from the provider.
func RenewProfile(ctx context.Context, profile ClientProfile, timeout time.Duration) (ClientProfile, error) {
	credentials, err := profile.Credentials()
	if err != nil {
		return ClientProfile{}, err
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	tlsConfig, err := RenewalTLSConfig(credentials)
	if err != nil {
		return ClientProfile{}, err
	}
	raw, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", profile.Endpoint)
	if err != nil {
		return ClientProfile{}, fmt.Errorf("connect to renewal service: %w", err)
	}
	defer raw.Close()
	conn := tls.Client(raw, tlsConfig)
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if err := conn.HandshakeContext(ctx); err != nil {
		return ClientProfile{}, fmt.Errorf("authenticate renewal service: %w", err)
	}
	if conn.ConnectionState().NegotiatedProtocol != RenewalALPN {
		return ClientProfile{}, errors.New("server did not negotiate Queqiao renewal")
	}
	if err := writeEnrollmentJSON(conn, renewalRequest{Version: ProfileVersion}); err != nil {
		return ClientProfile{}, err
	}
	responseBytes, err := readEnrollmentMessage(conn)
	if err != nil {
		return ClientProfile{}, fmt.Errorf("read renewal response: %w", err)
	}
	var response enrollmentResponse
	if err := decodeStrictJSON(responseBytes, &response); err != nil || response.Error != "" {
		if response.Error != "" {
			return ClientProfile{}, errors.New(response.Error)
		}
		return ClientProfile{}, errors.New("server returned an invalid renewal response")
	}
	if response.Version != ProfileVersion || response.ProviderID != profile.ProviderID ||
		response.GatewayID != profile.GatewayID || response.RootPin != profile.RootPin ||
		response.AccountID != profile.AccountID || response.DeviceID != profile.DeviceID {
		return ClientProfile{}, errors.New("renewal response identity mismatch")
	}
	renewed := profile
	renewed.DeviceCertificate = response.DeviceCertificate
	renewed.CreatedAt = response.IssuedAt
	newCredentials, err := renewed.Credentials()
	if err != nil {
		return ClientProfile{}, fmt.Errorf("validate renewed identity: %w", err)
	}
	oldLeaf, _ := x509.ParseCertificate(credentials.Certificate.Certificate[0])
	newLeaf, _ := x509.ParseCertificate(newCredentials.Certificate.Certificate[0])
	oldKey, oldOK := oldLeaf.PublicKey.(ed25519.PublicKey)
	newKey, newOK := newLeaf.PublicKey.(ed25519.PublicKey)
	if !oldOK || !newOK || !oldKey.Equal(newKey) || !newLeaf.NotAfter.After(oldLeaf.NotAfter) {
		return ClientProfile{}, errors.New("renewed certificate did not preserve the device key or extend its validity")
	}
	return renewed, nil
}

func (p ClientProfile) NeedsRenewal(now time.Time, renewalWindow time.Duration) (bool, error) {
	credentials, err := p.Credentials()
	if err != nil {
		return false, err
	}
	leaf, err := x509.ParseCertificate(credentials.Certificate.Certificate[0])
	if err != nil {
		return false, err
	}
	if renewalWindow <= 0 {
		renewalWindow = 7 * 24 * time.Hour
	}
	return !leaf.NotAfter.After(nowOrTime(now).Add(renewalWindow)), nil
}

// Enroll imports one invitation. The permanent device key is generated on the
// client and never appears in the invitation or provider state.
func Enroll(ctx context.Context, invitation Invitation, deviceName string, timeout time.Duration) (ClientProfile, error) {
	draft, err := NewEnrollmentDraft(invitation, deviceName)
	if err != nil {
		return ClientProfile{}, err
	}
	return draft.Enroll(ctx, timeout)
}

func (d EnrollmentDraft) Enroll(ctx context.Context, timeout time.Duration) (ClientProfile, error) {
	privateKey, err := d.privateKey()
	if err != nil {
		return ClientProfile{}, err
	}
	invitation, deviceName := d.Invitation, d.DeviceName
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	dialer := &net.Dialer{Timeout: timeout}
	raw, err := dialer.DialContext(ctx, "tcp", invitation.Endpoint)
	if err != nil {
		return ClientProfile{}, fmt.Errorf("connect to enrollment service: %w", err)
	}
	defer raw.Close()
	tlsConn := tls.Client(raw, EnrollmentTLSConfig(invitation.RootPin, invitation.ProviderID, invitation.GatewayID))
	deadline := time.Now().Add(timeout)
	_ = tlsConn.SetDeadline(deadline)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return ClientProfile{}, fmt.Errorf("verify enrollment service: %w", err)
	}
	if tlsConn.ConnectionState().NegotiatedProtocol != EnrollmentALPN {
		return ClientProfile{}, errors.New("server did not negotiate Queqiao enrollment")
	}
	request := enrollmentRequest{
		Version: InvitationVersion, Token: invitation.Token, DeviceName: deviceName,
		PublicKey: base64.RawURLEncoding.EncodeToString(publicKey),
	}
	if err := writeEnrollmentJSON(tlsConn, request); err != nil {
		return ClientProfile{}, fmt.Errorf("send enrollment request: %w", err)
	}
	responseBytes, err := readEnrollmentMessage(tlsConn)
	if err != nil {
		return ClientProfile{}, fmt.Errorf("read enrollment response: %w", err)
	}
	var response enrollmentResponse
	if err := decodeStrictJSON(responseBytes, &response); err != nil {
		return ClientProfile{}, errors.New("server returned an invalid enrollment response")
	}
	if response.Error != "" {
		return ClientProfile{}, errors.New(response.Error)
	}
	if response.Version != ProfileVersion || response.ProviderID != invitation.ProviderID ||
		response.GatewayID != invitation.GatewayID || response.RootPin != invitation.RootPin ||
		response.Endpoint != invitation.Endpoint || response.AccountID == "" || response.DeviceID == "" {
		return ClientProfile{}, errors.New("enrollment response identity mismatch")
	}
	profile := ClientProfile{
		Version: ProfileVersion, Name: response.ProviderName,
		ProviderID: response.ProviderID, Endpoint: response.Endpoint,
		GatewayID: response.GatewayID, RootPin: response.RootPin,
		RootCertificate: response.RootCertificate, AccountID: response.AccountID,
		DeviceID: response.DeviceID, DeviceName: deviceName,
		DeviceCertificate: response.DeviceCertificate,
		DevicePrivateKey:  encodeProfilePrivateKey(privateKey), CreatedAt: response.IssuedAt,
	}
	credentials, err := profile.Credentials()
	if err != nil {
		return ClientProfile{}, fmt.Errorf("validate enrolled identity: %w", err)
	}
	leaf, err := x509.ParseCertificate(credentials.Certificate.Certificate[0])
	if err != nil {
		return ClientProfile{}, fmt.Errorf("parse enrolled certificate: %w", err)
	}
	got, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok || !got.Equal(publicKey) {
		return ClientProfile{}, errors.New("issued certificate does not contain generated device key")
	}
	_ = tlsConn.SetDeadline(time.Time{})
	return profile, nil
}

func writeEnrollmentJSON(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data) > maxEnrollmentMessage {
		return errors.New("enrollment message exceeds limit")
	}
	var header [4]byte
	// maxEnrollmentMessage is far below MaxUint32 and is checked immediately
	// above, so this framing conversion cannot truncate.
	binary.BigEndian.PutUint32(header[:], uint32(len(data))) // #nosec G115
	if err := writeAll(w, header[:]); err != nil {
		return err
	}
	return writeAll(w, data)
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := w.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(data) {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func decodeStrictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("JSON message contains trailing data")
	}
	return nil
}

func readEnrollmentMessage(r io.Reader) ([]byte, error) {
	reader := bufio.NewReaderSize(r, maxEnrollmentMessage+4)
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(header[:])
	if length == 0 || length > maxEnrollmentMessage {
		return nil, errors.New("invalid enrollment message length")
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(reader, data); err != nil {
		return nil, err
	}
	return data, nil
}
