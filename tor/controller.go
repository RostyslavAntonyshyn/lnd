package tor

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/ioutil"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
)

const (
	// success is the Tor Control response code representing a successful
	// request.
	success = 250

	// nonceLen is the length of a nonce generated by either the controller
	// or the Tor server
	nonceLen = 32

	// cookieLen is the length of the authentication cookie.
	cookieLen = 32

	// ProtocolInfoVersion is the `protocolinfo` version currently supported
	// by the Tor server.
	ProtocolInfoVersion = 1

	// MinTorVersion is the minimum supported version that the Tor server
	// must be running on. This is needed in order to create v3 onion
	// services through Tor's control port.
	MinTorVersion = "0.3.3.6"
)

var (
	// serverKey is the key used when computing the HMAC-SHA256 of a message
	// from the server.
	serverKey = []byte("Tor safe cookie authentication " +
		"server-to-controller hash")

	// controllerKey is the key used when computing the HMAC-SHA256 of a
	// message from the controller.
	controllerKey = []byte("Tor safe cookie authentication " +
		"controller-to-server hash")
)

// Controller is an implementation of the Tor Control protocol. This is used in
// order to communicate with a Tor server. Its only supported method of
// authentication is the SAFECOOKIE method.
//
// NOTE: The connection to the Tor server must be authenticated before
// proceeding to send commands. Otherwise, the connection will be closed.
//
// TODO:
//   * if adding support for more commands, extend this with a command queue?
//   * place under sub-package?
//   * support async replies from the server
type Controller struct {
	// started is used atomically in order to prevent multiple calls to
	// Start.
	started int32

	// stopped is used atomically in order to prevent multiple calls to
	// Stop.
	stopped int32

	// conn is the underlying connection between the controller and the
	// Tor server. It provides read and write methods to simplify the
	// text-based messages within the connection.
	conn *textproto.Conn

	// controlAddr is the host:port the Tor server is listening locally for
	// controller connections on.
	controlAddr string

	// version is the current version of the Tor server.
	version string
}

// NewController returns a new Tor controller that will be able to interact with
// a Tor server.
func NewController(controlAddr string) *Controller {
	return &Controller{controlAddr: controlAddr}
}

// Start establishes and authenticates the connection between the controller and
// a Tor server. Once done, the controller will be able to send commands and
// expect responses.
func (c *Controller) Start() error {
	if !atomic.CompareAndSwapInt32(&c.started, 0, 1) {
		return nil
	}

	conn, err := textproto.Dial("tcp", c.controlAddr)
	if err != nil {
		return fmt.Errorf("unable to connect to Tor server: %v", err)
	}

	c.conn = conn

	return c.authenticate()
}

// Stop closes the connection between the controller and the Tor server.
func (c *Controller) Stop() error {
	if !atomic.CompareAndSwapInt32(&c.stopped, 0, 1) {
		return nil
	}

	return c.conn.Close()
}

// sendCommand sends a command to the Tor server and returns its response, as a
// single space-delimited string, and code.
func (c *Controller) sendCommand(command string) (int, string, error) {
	if err := c.conn.Writer.PrintfLine(command); err != nil {
		return 0, "", err
	}

	// We'll use ReadResponse as it has built-in support for multi-line
	// text protocol responses.
	code, reply, err := c.conn.Reader.ReadResponse(success)
	if err != nil {
		return code, reply, err
	}

	return code, reply, nil
}

// parseTorReply parses the reply from the Tor server after receiving a command
// from a controller. This will parse the relevant reply parameters into a map
// of keys and values.
func parseTorReply(reply string) map[string]string {
	params := make(map[string]string)

	// Replies can either span single or multiple lines, so we'll default
	// to stripping whitespace and newlines in order to retrieve the
	// individual contents of it. The -1 indicates that we want this to span
	// across all instances of a newline.
	contents := strings.Split(strings.Replace(reply, "\n", " ", -1), " ")
	for _, content := range contents {
		// Each parameter within the reply should be of the form
		// "KEY=VALUE". If the parameter doesn't contain "=", then we
		// can assume it does not provide any other relevant information
		// already known.
		keyValue := strings.Split(content, "=")
		if len(keyValue) != 2 {
			continue
		}

		key := keyValue[0]
		value := keyValue[1]
		params[key] = value
	}

	return params
}

// authenticate authenticates the connection between the controller and the
// Tor server using the SAFECOOKIE authentication method.
func (c *Controller) authenticate() error {
	// Before proceeding to authenticate the connection, we'll retrieve
	// the authentication cookie of the Tor server. This will be used
	// throughout the authentication routine. We do this before as once the
	// authentication routine has begun, it is not possible to retrieve it
	// mid-way.
	cookie, err := c.getAuthCookie()
	if err != nil {
		return fmt.Errorf("unable to retrieve authentication cookie: "+
			"%v", err)
	}

	// Authenticating using the SAFECOOKIE authentication method is a two
	// step process. We'll kick off the authentication routine by sending
	// the AUTHCHALLENGE command followed by a hex-encoded 32-byte nonce.
	clientNonce := make([]byte, nonceLen)
	if _, err := rand.Read(clientNonce); err != nil {
		return fmt.Errorf("unable to generate client nonce: %v", err)
	}

	cmd := fmt.Sprintf("AUTHCHALLENGE SAFECOOKIE %x", clientNonce)
	_, reply, err := c.sendCommand(cmd)
	if err != nil {
		return err
	}

	// If successful, the reply from the server should be of the following
	// format:
	//
	//	"250 AUTHCHALLENGE"
	//		SP "SERVERHASH=" ServerHash
	//		SP "SERVERNONCE=" ServerNonce
	//		CRLF
	//
	// We're interested in retrieving the SERVERHASH and SERVERNONCE
	// parameters, so we'll parse our reply to do so.
	replyParams := parseTorReply(reply)

	// Once retrieved, we'll ensure these values are of proper length when
	// decoded.
	serverHash, ok := replyParams["SERVERHASH"]
	if !ok {
		return errors.New("server hash not found in reply")
	}
	decodedServerHash, err := hex.DecodeString(serverHash)
	if err != nil {
		return fmt.Errorf("unable to decode server hash: %v", err)
	}
	if len(decodedServerHash) != sha256.Size {
		return errors.New("invalid server hash length")
	}

	serverNonce, ok := replyParams["SERVERNONCE"]
	if !ok {
		return errors.New("server nonce not found in reply")
	}
	decodedServerNonce, err := hex.DecodeString(serverNonce)
	if err != nil {
		return fmt.Errorf("unable to decode server nonce: %v", err)
	}
	if len(decodedServerNonce) != nonceLen {
		return errors.New("invalid server nonce length")
	}

	// The server hash above was constructed by computing the HMAC-SHA256
	// of the message composed of the cookie, client nonce, and server
	// nonce. We'll redo this computation ourselves to ensure the integrity
	// and authentication of the message.
	hmacMessage := bytes.Join(
		[][]byte{cookie, clientNonce, decodedServerNonce}, []byte{},
	)
	computedServerHash := computeHMAC256(serverKey, hmacMessage)
	if !hmac.Equal(computedServerHash, decodedServerHash) {
		return fmt.Errorf("expected server hash %x, got %x",
			decodedServerHash, computedServerHash)
	}

	// If the MAC check was successful, we'll proceed with the last step of
	// the authentication routine. We'll now send the AUTHENTICATE command
	// followed by a hex-encoded client hash constructed by computing the
	// HMAC-SHA256 of the same message, but this time using the controller's
	// key.
	clientHash := computeHMAC256(controllerKey, hmacMessage)
	if len(clientHash) != sha256.Size {
		return errors.New("invalid client hash length")
	}

	cmd = fmt.Sprintf("AUTHENTICATE %x", clientHash)
	if _, _, err := c.sendCommand(cmd); err != nil {
		return err
	}

	return nil
}

// getAuthCookie retrieves the authentication cookie in bytes from the Tor
// server. Cookie authentication must be enabled for this to work. The boolean
func (c *Controller) getAuthCookie() ([]byte, error) {
	// Retrieve the authentication methods currently supported by the Tor
	// server.
	authMethods, cookieFilePath, version, err := c.ProtocolInfo()
	if err != nil {
		return nil, err
	}

	// With the version retrieved, we'll cache it now in case it needs to be
	// used later on.
	c.version = version

	// Ensure that the Tor server supports the SAFECOOKIE authentication
	// method.
	safeCookieSupport := false
	for _, authMethod := range authMethods {
		if authMethod == "SAFECOOKIE" {
			safeCookieSupport = true
		}
	}

	if !safeCookieSupport {
		return nil, errors.New("the Tor server is currently not " +
			"configured for cookie authentication")
	}

	// Read the cookie from the file and ensure it has the correct length.
	cookie, err := ioutil.ReadFile(cookieFilePath)
	if err != nil {
		return nil, err
	}

	if len(cookie) != cookieLen {
		return nil, errors.New("invalid authentication cookie length")
	}

	return cookie, nil
}

// computeHMAC256 computes the HMAC-SHA256 of a key and message.
func computeHMAC256(key, message []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(message)
	return mac.Sum(nil)
}

// supportsV3 is a helper function that parses the current version of the Tor
// server and determines whether it supports creationg v3 onion services through
// Tor's control port. The version string should be of the format:
//	major.minor.revision.build
func supportsV3(version string) error {
	// We'll split the minimum Tor version that's supported and the given
	// version in order to individually compare each number.
	requiredParts := strings.Split(MinTorVersion, ".")
	parts := strings.Split(version, ".")
	if len(parts) != 4 {
		return errors.New("version string is not of the format " +
			"major.minor.revision.build")
	}

	// It's possible that the build number (the last part of the version
	// string) includes a pre-release string, e.g. rc, beta, etc., so we'll
	// parse that as well.
	build := strings.Split(parts[len(parts)-1], "-")
	parts[len(parts)-1] = build[0]

	// Convert them each number from its string representation to integers
	// and check that they respect the minimum version.
	for i := range parts {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return err
		}
		requiredN, err := strconv.Atoi(requiredParts[i])
		if err != nil {
			return err
		}

		if n < requiredN {
			return fmt.Errorf("version %v below minimum version "+
				"supported %v", version, MinTorVersion)
		}
	}

	return nil
}

// ProtocolInfo returns the different authentication methods supported by the
// Tor server and the version of the Tor server.
func (c *Controller) ProtocolInfo() ([]string, string, string, error) {
	// We'll start off by sending the "PROTOCOLINFO" command to the Tor
	// server. We should receive a reply of the following format:
	//
	//	METHODS=COOKIE,SAFECOOKIE
	//	COOKIEFILE="/home/user/.tor/control_auth_cookie"
	//	VERSION Tor="0.3.2.10"
	//
	// We're interested in retrieving all of these fields, so we'll parse
	// our reply to do so.
	cmd := fmt.Sprintf("PROTOCOLINFO %d", ProtocolInfoVersion)
	_, reply, err := c.sendCommand(cmd)
	if err != nil {
		return nil, "", "", err
	}

	info := parseTorReply(reply)
	methods, ok := info["METHODS"]
	if !ok {
		return nil, "", "", errors.New("auth methods not found in " +
			"reply")
	}

	cookieFile, ok := info["COOKIEFILE"]
	if !ok {
		return nil, "", "", errors.New("cookie file path not found " +
			"in reply")
	}

	version, ok := info["Tor"]
	if !ok {
		return nil, "", "", errors.New("Tor version not found in reply")
	}

	// Finally, we'll clean up the results before returning them.
	authMethods := strings.Split(methods, ",")
	cookieFilePath := strings.Trim(cookieFile, "\"")
	torVersion := strings.Trim(version, "\"")

	return authMethods, cookieFilePath, torVersion, nil
}

// OnionType denotes the type of the onion service.
type OnionType int

const (
	// V2 denotes that the onion service is V2.
	V2 OnionType = iota

	// V3 denotes that the onion service is V3.
	V3
)

// AddOnionConfig houses all of the required paramaters in order to succesfully
// create a new onion service or restore an existing one.
type AddOnionConfig struct {
	// Type denotes the type of the onion service that should be created.
	Type OnionType

	// VirtualPort is the externally reachable port of the onion address.
	VirtualPort int

	// TargetPorts is the set of ports that the service will be listening on
	// locally. The Tor server will use choose a random port from this set
	// to forward the traffic from the virtual port.
	//
	// NOTE: If nil/empty, the virtual port will be used as the only target
	// port.
	TargetPorts []int

	// PrivateKeyPath is the full path to where the onion service's private
	// key is stored. This can be used to restore an existing onion service.
	PrivateKeyPath string
}

// AddOnion creates an onion service and returns its onion address. Once
// created, the new onion service will remain active until the connection
// between the controller and the Tor server is closed.
func (c *Controller) AddOnion(cfg AddOnionConfig) (*OnionAddr, error) {
	// Before sending the request to create an onion service to the Tor
	// server, we'll make sure that it supports V3 onion services if that
	// was the type requested.
	if cfg.Type == V3 {
		if err := supportsV3(c.version); err != nil {
			return nil, err
		}
	}

	// We'll start off by checking if the file containing the private key
	// exists. If it does not, then we should request the server to create
	// a new onion service and return its private key. Otherwise, we'll
	// request the server to recreate the onion server from our private key.
	var keyParam string
	if _, err := os.Stat(cfg.PrivateKeyPath); os.IsNotExist(err) {
		switch cfg.Type {
		case V2:
			keyParam = "NEW:RSA1024"
		case V3:
			keyParam = "NEW:ED25519-V3"
		}
	} else {
		privateKey, err := ioutil.ReadFile(cfg.PrivateKeyPath)
		if err != nil {
			return nil, err
		}
		keyParam = string(privateKey)
	}

	// Now, we'll create a mapping from the virtual port to each target
	// port. If no target ports were specified, we'll use the virtual port
	// to provide a one-to-one mapping.
	var portParam string
	if len(cfg.TargetPorts) == 0 {
		portParam += fmt.Sprintf("Port=%d,%d ", cfg.VirtualPort,
			cfg.VirtualPort)
	} else {
		for _, targetPort := range cfg.TargetPorts {
			portParam += fmt.Sprintf("Port=%d,%d ", cfg.VirtualPort,
				targetPort)
		}
	}

	// Send the command to create the onion service to the Tor server and
	// await its response.
	cmd := fmt.Sprintf("ADD_ONION %s %s", keyParam, portParam)
	_, reply, err := c.sendCommand(cmd)
	if err != nil {
		return nil, err
	}

	// If successful, the reply from the server should be of the following
	// format, depending on whether a private key has been requested:
	//
	//	C: ADD_ONION RSA1024:[Blob Redacted] Port=80,8080
	//	S: 250-ServiceID=testonion1234567
	//	S: 250 OK
	//
	//	C: ADD_ONION NEW:RSA1024 Port=80,8080
	//	S: 250-ServiceID=testonion1234567
	//	S: 250-PrivateKey=RSA1024:[Blob Redacted]
	//	S: 250 OK
	//
	// We're interested in retrieving the service ID, which is the public
	// name of the service, and the private key if requested.
	replyParams := parseTorReply(reply)
	serviceID, ok := replyParams["ServiceID"]
	if !ok {
		return nil, errors.New("service id not found in reply")
	}

	// If a new onion service was created, we'll write its private key to
	// disk under strict permissions in the event that it needs to be
	// recreated later on.
	if privateKey, ok := replyParams["PrivateKey"]; ok {
		err := ioutil.WriteFile(
			cfg.PrivateKeyPath, []byte(privateKey), 0600,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to write private key "+
				"to file: %v", err)
		}
	}

	// Finally, we'll return the onion address composed of the service ID,
	// along with the onion suffix, and the port this onion service can be
	// reached at externally.
	return &OnionAddr{
		OnionService: serviceID + ".onion",
		Port:         cfg.VirtualPort,
	}, nil
}
