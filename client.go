// Package gomatrix implements the Matrix Client-Server API.
//
// Specification can be found at http://matrix.org/docs/spec/client_server/r0.2.0.html
package gomatrix

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"sync"
	"time"
)

// Client represents a Matrix client.
type Client struct {
	HomeserverURL *url.URL     // The base homeserver URL
	Prefix        string       // The API prefix eg '/_matrix/client/r0'
	UserID        string       // The user ID of the client. Used for forming HTTP paths which use the client's user ID.
	AccessToken   string       // The access_token for the client.
	Client        *http.Client // The underlying HTTP client which will be used to make HTTP requests.
	Syncer        Syncer       // The thing which can process /sync responses
	Store         Storer       // The thing which can store rooms/tokens/ids

	// The ?user_id= query parameter for application services. This must be set *prior* to calling a method. If this is empty,
	// no user_id parameter will be sent.
	// See http://matrix.org/docs/spec/application_service/unstable.html#identity-assertion
	AppServiceUserID string

	syncingMutex sync.Mutex // protects syncingID
	syncingID    uint32     // Identifies the current Sync. Only one Sync can be active at any given time.
}

// HTTPError An HTTP Error response, which may wrap an underlying native Go Error.
type HTTPError struct {
	WrappedError error
	Message      string
	Code         int
}

func (e HTTPError) Error() string {
	var wrappedErrMsg string
	if e.WrappedError != nil {
		wrappedErrMsg = e.WrappedError.Error()
	}
	return fmt.Sprintf("msg=%s code=%d wrapped=%s", e.Message, e.Code, wrappedErrMsg)
}

// BuildURL builds a URL with the Client's homserver/prefix/access_token set already.
func (cli *Client) BuildURL(urlPath ...string) string {
	ps := []string{cli.Prefix}
	for _, p := range urlPath {
		ps = append(ps, p)
	}
	return cli.BuildBaseURL(ps...)
}

// BuildBaseURL builds a URL with the Client's homeserver/access_token set already. You must
// supply the prefix in the path.
func (cli *Client) BuildBaseURL(urlPath ...string) string {
	// copy the URL. Purposefully ignore error as the input is from a valid URL already
	hsURL, _ := url.Parse(cli.HomeserverURL.String())
	parts := []string{hsURL.Path}
	parts = append(parts, urlPath...)
	hsURL.Path = path.Join(parts...)
	query := hsURL.Query()
	if cli.AccessToken != "" {
		query.Set("access_token", cli.AccessToken)
	}
	if cli.AppServiceUserID != "" {
		query.Set("user_id", cli.AppServiceUserID)
	}
	hsURL.RawQuery = query.Encode()
	return hsURL.String()
}

// BuildURLWithQuery builds a URL with query paramters in addition to the Client's homeserver/prefix/access_token set already.
func (cli *Client) BuildURLWithQuery(urlPath []string, urlQuery map[string]string) string {
	u, _ := url.Parse(cli.BuildURL(urlPath...))
	q := u.Query()
	for k, v := range urlQuery {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// SetCredentials sets the user ID and access token on this client instance.
func (cli *Client) SetCredentials(userID, accessToken string) {
	cli.AccessToken = accessToken
	cli.UserID = userID
}

// ClearCredentials removes the user ID and access token on this client instance.
func (cli *Client) ClearCredentials() {
	cli.AccessToken = ""
	cli.UserID = ""
}

// Sync starts syncing with the provided Homeserver. If Sync() is called twice then the first sync will be stopped and the
// error will be nil.
//
// This function will block until a fatal /sync error occurs, so it should almost always be started as a new goroutine.
// Fatal sync errors can be caused by:
//   - The failure to create a filter.
//   - Client.Syncer.OnFailedSync returning an error in response to a failed sync.
//   - Client.Syncer.ProcessResponse returning an error.
// If you wish to continue retrying in spite of these fatal errors, call Sync() again.
func (cli *Client) Sync() error {
	// Mark the client as syncing.
	// We will keep syncing until the syncing state changes. Either because
	// Sync is called or StopSync is called.
	syncingID := cli.incrementSyncingID()
	nextBatch := cli.Store.LoadNextBatch(cli.UserID)
	filterID := cli.Store.LoadFilterID(cli.UserID)
	if filterID == "" {
		filterJSON := cli.Syncer.GetFilterJSON(cli.UserID)
		resFilter, err := cli.CreateFilter(filterJSON)
		if err != nil {
			return err
		}
		filterID = resFilter.FilterID
		cli.Store.SaveFilterID(cli.UserID, filterID)
	}

	for {
		resSync, err := cli.SyncRequest(30000, nextBatch, filterID, false, "")
		if err != nil {
			duration, err2 := cli.Syncer.OnFailedSync(resSync, err)
			if err2 != nil {
				return err2
			}
			time.Sleep(duration)
			continue
		}

		// Check that the syncing state hasn't changed
		// Either because we've stopped syncing or another sync has been started.
		// We discard the response from our sync.
		if cli.getSyncingID() != syncingID {
			return nil
		}

		// Save the token now *before* processing it. This means it's possible
		// to not process some events, but it means that we won't get constantly stuck processing
		// a malformed/buggy event which keeps making us panic.
		cli.Store.SaveNextBatch(cli.UserID, resSync.NextBatch)
		if err = cli.Syncer.ProcessResponse(resSync, nextBatch); err != nil {
			return err
		}

		nextBatch = resSync.NextBatch
	}
}

func (cli *Client) incrementSyncingID() uint32 {
	cli.syncingMutex.Lock()
	defer cli.syncingMutex.Unlock()
	cli.syncingID++
	return cli.syncingID
}

func (cli *Client) getSyncingID() uint32 {
	cli.syncingMutex.Lock()
	defer cli.syncingMutex.Unlock()
	return cli.syncingID
}

// StopSync stops the ongoing sync started by Sync.
func (cli *Client) StopSync() {
	// Advance the syncing state so that any running Syncs will terminate.
	cli.incrementSyncingID()
}

// MakeRequest makes a JSON HTTP request to the given URL.
// If "resBody" is not nil, the response body will be json.Unmarshalled into it.
//
// Returns the HTTP body as bytes on 2xx with a nil error. Returns an error if the response is not 2xx along
// with the HTTP body bytes if it got that far. This error is an HTTPError which includes the returned
// HTTP status code and possibly a RespError as the WrappedError, if the HTTP body could be decoded as a RespError.
func (cli *Client) MakeRequest(method string, httpURL string, reqBody interface{}, resBody interface{}) ([]byte, error) {
	var req *http.Request
	var err error
	if reqBody != nil {
		var jsonStr []byte
		jsonStr, err = json.Marshal(reqBody)
		if err != nil {
			return nil, err
		}
		req, err = http.NewRequest(method, httpURL, bytes.NewBuffer(jsonStr))
	} else {
		req, err = http.NewRequest(method, httpURL, nil)
	}

	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := cli.Client.Do(req)
	if res != nil {
		defer res.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	contents, err := ioutil.ReadAll(res.Body)
	if res.StatusCode/100 != 2 { // not 2xx
		var wrap error
		var respErr RespError
		if _ = json.Unmarshal(contents, &respErr); respErr.ErrCode != "" {
			wrap = respErr
		}

		// If we failed to decode as RespError, don't just drop the HTTP body, include it in the
		// HTTP error instead (e.g proxy errors which return HTML).
		msg := "Failed to " + method + " JSON to " + req.URL.Path
		if wrap == nil {
			msg = msg + ": " + string(contents)
		}

		return contents, HTTPError{
			Code:         res.StatusCode,
			Message:      msg,
			WrappedError: wrap,
		}
	}
	if err != nil {
		return nil, err
	}

	if resBody != nil {
		if err = json.Unmarshal(contents, &resBody); err != nil {
			return nil, err
		}
	}

	return contents, nil
}

// CreateFilter makes an HTTP request according to http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-user-userid-filter
func (cli *Client) CreateFilter(filter json.RawMessage) (resp *RespCreateFilter, err error) {
	urlPath := cli.BuildURL("user", cli.UserID, "filter")
	_, err = cli.MakeRequest("POST", urlPath, &filter, &resp)
	return
}

// SyncRequest makes an HTTP request according to http://matrix.org/docs/spec/client_server/r0.2.0.html#get-matrix-client-r0-sync
func (cli *Client) SyncRequest(timeout int, since, filterID string, fullState bool, setPresence string) (resp *RespSync, err error) {
	query := map[string]string{
		"timeout": strconv.Itoa(timeout),
	}
	if since != "" {
		query["since"] = since
	}
	if filterID != "" {
		query["filter"] = filterID
	}
	if setPresence != "" {
		query["set_presence"] = setPresence
	}
	if fullState {
		query["full_state"] = "true"
	}
	urlPath := cli.BuildURLWithQuery([]string{"sync"}, query)
	_, err = cli.MakeRequest("GET", urlPath, nil, &resp)
	return
}

func (cli *Client) register(u string, req *ReqRegister) (resp *RespRegister, uiaResp *RespUserInteractive, err error) {
	var bodyBytes []byte
	bodyBytes, err = cli.MakeRequest("POST", u, req, nil)
	if err != nil {
		httpErr, ok := err.(HTTPError)
		if !ok { // network error
			return
		}
		if httpErr.Code == 401 {
			// body should be RespUserInteractive, if it isn't, fail with the error
			err = json.Unmarshal(bodyBytes, &uiaResp)
			return
		}
		return
	}
	// body should be RespRegister
	err = json.Unmarshal(bodyBytes, &resp)
	return
}

// Register makes an HTTP request according to http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-register
//
// Registers with kind=user. For kind=guest, see RegisterGuest.
func (cli *Client) Register(req *ReqRegister) (*RespRegister, *RespUserInteractive, error) {
	u := cli.BuildURL("register")
	return cli.register(u, req)
}

// RegisterGuest makes an HTTP request according to http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-register
// with kind=guest.
//
// For kind=user, see Register.
func (cli *Client) RegisterGuest(req *ReqRegister) (*RespRegister, *RespUserInteractive, error) {
	query := map[string]string{
		"kind": "guest",
	}
	u := cli.BuildURLWithQuery([]string{"register"}, query)
	return cli.register(u, req)
}

// RegisterDummy performs m.login.dummy registration according to https://matrix.org/docs/spec/client_server/r0.2.0.html#dummy-auth
//
// Only a username and password need to be provided on the ReqRegister struct. Most local/developer homeservers will allow registration
// this way. If the homeserver does not, an error is returned.
//
// This does not set credentials on the client instance. See SetCredentials() instead.
//
// 	res, err := cli.RegisterDummy(&gomatrix.ReqRegister{
//		Username: "alice",
//		Password: "wonderland",
//	})
//  if err != nil {
// 		panic(err)
// 	}
// 	token := res.AccessToken
func (cli *Client) RegisterDummy(req *ReqRegister) (*RespRegister, error) {
	res, uia, err := cli.Register(req)
	if err != nil && uia == nil {
		return nil, err
	}
	if uia != nil && uia.HasSingleStageFlow("m.login.dummy") {
		req.Auth = struct {
			Type    string `json:"type"`
			Session string `json:"session,omitempty"`
		}{"m.login.dummy", uia.Session}
		res, _, err = cli.Register(req)
		if err != nil {
			return nil, err
		}
	}
	if res == nil {
		return nil, fmt.Errorf("registration failed: does this server support m.login.dummy?")
	}
	return res, nil
}

// Login a user to the homeserver according to http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-login
// This does not set credentials on this client instance. See SetCredentials() instead.
func (cli *Client) Login(req *ReqLogin) (resp *RespLogin, err error) {
	urlPath := cli.BuildURL("login")
	_, err = cli.MakeRequest("POST", urlPath, req, &resp)
	return
}

// Logout the current user. See http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-logout
// This does not clear the credentials from the client instance. See ClearCredentials() instead.
func (cli *Client) Logout() (resp *RespLogout, err error) {
	urlPath := cli.BuildURL("logout")
	_, err = cli.MakeRequest("POST", urlPath, nil, &resp)
	return
}

// Versions returns the list of supported Matrix versions on this homeserver. See http://matrix.org/docs/spec/client_server/r0.2.0.html#get-matrix-client-versions
func (cli *Client) Versions() (resp *RespVersions, err error) {
	urlPath := cli.BuildBaseURL("_matrix", "client", "versions")
	_, err = cli.MakeRequest("GET", urlPath, nil, &resp)
	return
}

// JoinRoom joins the client to a room ID or alias. See http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-join-roomidoralias
//
// If serverName is specified, this will be added as a query param to instruct the homeserver to join via that server. If content is specified, it will
// be JSON encoded and used as the request body.
func (cli *Client) JoinRoom(roomIDorAlias, serverName string, content interface{}) (resp *RespJoinRoom, err error) {
	var urlPath string
	if serverName != "" {
		urlPath = cli.BuildURLWithQuery([]string{"join", roomIDorAlias}, map[string]string{
			"server_name": serverName,
		})
	} else {
		urlPath = cli.BuildURL("join", roomIDorAlias)
	}
	_, err = cli.MakeRequest("POST", urlPath, content, &resp)
	return
}

// SetDisplayName sets the user's profile display name. See http://matrix.org/docs/spec/client_server/r0.2.0.html#put-matrix-client-r0-profile-userid-displayname
func (cli *Client) SetDisplayName(displayName string) (err error) {
	urlPath := cli.BuildURL("profile", cli.UserID, "displayname")
	s := struct {
		DisplayName string `json:"displayname"`
	}{displayName}
	_, err = cli.MakeRequest("PUT", urlPath, &s, nil)
	return
}

// GetAvatarURL gets the user's avatar URL. See http://matrix.org/docs/spec/client_server/r0.2.0.html#get-matrix-client-r0-profile-userid-avatar-url
func (cli *Client) GetAvatarURL() (url string, err error) {
	urlPath := cli.BuildURL("profile", cli.UserID, "avatar_url")
	s := struct {
		AvatarURL string `json:"avatar_url"`
	}{}

	_, err = cli.MakeRequest("GET", urlPath, nil, &s)
	if err != nil {
		return "", err
	}

	return s.AvatarURL, nil
}

// SetAvatarURL sets the user's avatar URL. See http://matrix.org/docs/spec/client_server/r0.2.0.html#put-matrix-client-r0-profile-userid-avatar-url
func (cli *Client) SetAvatarURL(url string) (err error) {
	urlPath := cli.BuildURL("profile", cli.UserID, "avatar_url")
	s := struct {
		AvatarURL string `json:"avatar_url"`
	}{url}
	_, err = cli.MakeRequest("PUT", urlPath, &s, nil)
	if err != nil {
		return err
	}

	return nil
}

// SendMessageEvent sends a message event into a room. See http://matrix.org/docs/spec/client_server/r0.2.0.html#put-matrix-client-r0-rooms-roomid-send-eventtype-txnid
// contentJSON should be a pointer to something that can be encoded as JSON using json.Marshal.
func (cli *Client) SendMessageEvent(roomID string, eventType string, contentJSON interface{}) (resp *RespSendEvent, err error) {
	txnID := txnID()
	urlPath := cli.BuildURL("rooms", roomID, "send", eventType, txnID)
	_, err = cli.MakeRequest("PUT", urlPath, contentJSON, &resp)
	return
}

// SendStateEvent sends a state event into a room. See http://matrix.org/docs/spec/client_server/r0.2.0.html#put-matrix-client-r0-rooms-roomid-state-eventtype-statekey
// contentJSON should be a pointer to something that can be encoded as JSON using json.Marshal.
func (cli *Client) SendStateEvent(roomID, eventType, stateKey string, contentJSON interface{}) (resp *RespSendEvent, err error) {
	urlPath := cli.BuildURL("rooms", roomID, "state", eventType, stateKey)
	_, err = cli.MakeRequest("PUT", urlPath, contentJSON, &resp)
	return
}

// SendText sends an m.room.message event into the given room with a msgtype of m.text
// See http://matrix.org/docs/spec/client_server/r0.2.0.html#m-text
func (cli *Client) SendText(roomID, text string) (*RespSendEvent, error) {
	return cli.SendMessageEvent(roomID, "m.room.message",
		TextMessage{"m.text", text})
}

// SendNotice sends an m.room.message event into the given room with a msgtype of m.notice
// See http://matrix.org/docs/spec/client_server/r0.2.0.html#m-notice
func (cli *Client) SendNotice(roomID, text string) (*RespSendEvent, error) {
	return cli.SendMessageEvent(roomID, "m.room.message",
		TextMessage{"m.notice", text})
}

// RedactEvent redacts the given event. See http://matrix.org/docs/spec/client_server/r0.2.0.html#put-matrix-client-r0-rooms-roomid-redact-eventid-txnid
func (cli *Client) RedactEvent(roomID, eventID string, req *ReqRedact) (resp *RespSendEvent, err error) {
	txnID := txnID()
	urlPath := cli.BuildURL("rooms", roomID, "redact", eventID, txnID)
	_, err = cli.MakeRequest("PUT", urlPath, req, &resp)
	return
}

// CreateRoom creates a new Matrix room. See https://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-createroom
//  resp, err := cli.CreateRoom(&gomatrix.ReqCreateRoom{
//  	Preset: "public_chat",
//  })
//  fmt.Println("Room:", resp.RoomID)
func (cli *Client) CreateRoom(req *ReqCreateRoom) (resp *RespCreateRoom, err error) {
	urlPath := cli.BuildURL("createRoom")
	_, err = cli.MakeRequest("POST", urlPath, req, &resp)
	return
}

// LeaveRoom leaves the given room. See http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-rooms-roomid-leave
func (cli *Client) LeaveRoom(roomID string) (resp *RespLeaveRoom, err error) {
	u := cli.BuildURL("rooms", roomID, "leave")
	_, err = cli.MakeRequest("POST", u, struct{}{}, &resp)
	return
}

// ForgetRoom forgets a room entirely. See http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-rooms-roomid-forget
func (cli *Client) ForgetRoom(roomID string) (resp *RespForgetRoom, err error) {
	u := cli.BuildURL("rooms", roomID, "forget")
	_, err = cli.MakeRequest("POST", u, struct{}{}, &resp)
	return
}

// InviteUser invites a user to a room. See http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-rooms-roomid-invite
func (cli *Client) InviteUser(roomID string, req *ReqInviteUser) (resp *RespInviteUser, err error) {
	u := cli.BuildURL("rooms", roomID, "invite")
	_, err = cli.MakeRequest("POST", u, struct{}{}, &resp)
	return
}

// InviteUserByThirdParty invites a third-party identifier to a room. See http://matrix.org/docs/spec/client_server/r0.2.0.html#invite-by-third-party-id-endpoint
func (cli *Client) InviteUserByThirdParty(roomID string, req *ReqInvite3PID) (resp *RespInviteUser, err error) {
	u := cli.BuildURL("rooms", roomID, "invite")
	_, err = cli.MakeRequest("POST", u, req, &resp)
	return
}

// KickUser kicks a user from a room. See http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-rooms-roomid-kick
func (cli *Client) KickUser(roomID string, req *ReqKickUser) (resp *RespKickUser, err error) {
	u := cli.BuildURL("rooms", roomID, "kick")
	_, err = cli.MakeRequest("POST", u, req, &resp)
	return
}

// BanUser bans a user from a room. See http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-rooms-roomid-ban
func (cli *Client) BanUser(roomID string, req *ReqBanUser) (resp *RespBanUser, err error) {
	u := cli.BuildURL("rooms", roomID, "ban")
	_, err = cli.MakeRequest("POST", u, req, &resp)
	return
}

// UnbanUser unbans a user from a room. See http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-client-r0-rooms-roomid-unban
func (cli *Client) UnbanUser(roomID string, req *ReqUnbanUser) (resp *RespUnbanUser, err error) {
	u := cli.BuildURL("rooms", roomID, "unban")
	_, err = cli.MakeRequest("POST", u, req, &resp)
	return
}

// UserTyping sets the typing status of the user. See https://matrix.org/docs/spec/client_server/r0.2.0.html#put-matrix-client-r0-rooms-roomid-typing-userid
func (cli *Client) UserTyping(roomID string, typing bool, timeout int64) (resp *RespTyping, err error) {
	req := ReqTyping{Typing: typing, Timeout: timeout}
	u := cli.BuildURL("rooms", roomID, "typing", cli.UserID)
	_, err = cli.MakeRequest("PUT", u, req, &resp)
	return
}

// StateEvent gets a single state event in a room. It will attempt to JSON unmarshal into the given "outContent" struct with
// the HTTP response body, or return an error.
// See http://matrix.org/docs/spec/client_server/r0.2.0.html#get-matrix-client-r0-rooms-roomid-state-eventtype-statekey
func (cli *Client) StateEvent(roomID, eventType, stateKey string, outContent interface{}) (err error) {
	u := cli.BuildURL("rooms", roomID, "state", eventType, stateKey)
	_, err = cli.MakeRequest("GET", u, nil, outContent)
	return
}

// UploadLink uploads an HTTP URL and then returns an MXC URI.
func (cli *Client) UploadLink(link string) (*RespMediaUpload, error) {
	res, err := cli.Client.Get(link)
	if res != nil {
		defer res.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	return cli.UploadToContentRepo(res.Body, res.Header.Get("Content-Type"), res.ContentLength)
}

// UploadToContentRepo uploads the given bytes to the content repository and returns an MXC URI.
// See http://matrix.org/docs/spec/client_server/r0.2.0.html#post-matrix-media-r0-upload
func (cli *Client) UploadToContentRepo(content io.Reader, contentType string, contentLength int64) (*RespMediaUpload, error) {
	req, err := http.NewRequest("POST", cli.BuildBaseURL("_matrix/media/r0/upload"), content)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = contentLength
	res, err := cli.Client.Do(req)
	if res != nil {
		defer res.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	if res.StatusCode != 200 {
		contents, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return nil, HTTPError{
				Message: "Upload request failed - Failed to read response body: " + err.Error(),
				Code:    res.StatusCode,
			}
		}
		return nil, HTTPError{
			Message: "Upload request failed: " + string(contents),
			Code:    res.StatusCode,
		}
	}
	var m RespMediaUpload
	if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// JoinedMembers returns a map of joined room members. See TODO-SPEC. https://github.com/matrix-org/synapse/pull/1680
//
// In general, usage of this API is discouraged in favour of /sync, as calling this API can race with incoming membership changes.
// This API is primarily designed for application services which may want to efficiently look up joined members in a room.
func (cli *Client) JoinedMembers(roomID string) (resp *RespJoinedMembers, err error) {
	u := cli.BuildURL("rooms", roomID, "joined_members")
	_, err = cli.MakeRequest("GET", u, nil, &resp)
	return
}

// JoinedRooms returns a list of rooms which the client is joined to. See TODO-SPEC. https://github.com/matrix-org/synapse/pull/1680
//
// In general, usage of this API is discouraged in favour of /sync, as calling this API can race with incoming membership changes.
// This API is primarily designed for application services which may want to efficiently look up joined rooms.
func (cli *Client) JoinedRooms() (resp *RespJoinedRooms, err error) {
	u := cli.BuildURL("joined_rooms")
	_, err = cli.MakeRequest("GET", u, nil, &resp)
	return
}

// Messages returns a list of message and state events for a room. It uses
// pagination query parameters to paginate history in the room.
// See https://matrix.org/docs/spec/client_server/r0.2.0.html#get-matrix-client-r0-rooms-roomid-messages
func (cli *Client) Messages(roomID, from, to string, dir rune, limit int) (resp *RespMessages, err error) {
	query := map[string]string{
		"from": from,
		"dir":  string(dir),
	}
	if to != "" {
		query["to"] = to
	}
	if limit != 0 {
		query["limit"] = string(limit)
	}

	urlPath := cli.BuildURLWithQuery([]string{"rooms", roomID, "messages"}, query)
	_, err = cli.MakeRequest("GET", urlPath, nil, &resp)
	return
}

func txnID() string {
	return "go" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

// NewClient creates a new Matrix Client ready for syncing
func NewClient(homeserverURL, userID, accessToken string) (*Client, error) {
	hsURL, err := url.Parse(homeserverURL)
	if err != nil {
		return nil, err
	}
	// By default, use an in-memory store which will never save filter ids / next batch tokens to disk.
	// The client will work with this storer: it just won't remember across restarts.
	// In practice, a database backend should be used.
	store := NewInMemoryStore()
	cli := Client{
		AccessToken:   accessToken,
		HomeserverURL: hsURL,
		UserID:        userID,
		Prefix:        "/_matrix/client/r0",
		Syncer:        NewDefaultSyncer(userID, store),
		Store:         store,
	}
	// By default, use the default HTTP client.
	cli.Client = http.DefaultClient

	return &cli, nil
}
