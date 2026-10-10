//go:build !ios_extension && !sdk_mobile_bind

// Getters for the session controller's snapshot types, for hosts that bind
// methods only. The gomobile view leaves them out for the reason given in
// api_sessions_native.go: gobind binds the fields themselves.
package sdk

func (self *ClientSessionError) GetMessage() string { return self.Message }

func (self *ClientSessionError) GetRetryable() bool { return self.Retryable }

func (self *ClientSessionError) GetSignInRequired() bool { return self.SignInRequired }

func (self *ClientSessionError) GetUnsupported() bool { return self.Unsupported }

func (self *ClientSessionAction) GetSessionId() *Id { return self.SessionId }

func (self *ClientSessionAction) GetOperationId() *Id { return self.OperationId }

func (self *ClientSessionAction) GetLoading() bool { return self.Loading }

func (self *ClientSessionAction) GetPending() bool { return self.Pending }

func (self *ClientSessionAction) GetError() *ClientSessionError { return self.Error }

func (self *ClientSessionSnapshot) GetSessions() *NetworkSessionInfoList { return self.Sessions }

func (self *ClientSessionSnapshot) GetCurrentSessionId() *Id { return self.CurrentSessionId }

func (self *ClientSessionSnapshot) GetLegacyCoverage() string { return self.LegacyCoverage }

func (self *ClientSessionSnapshot) GetGeneration() string { return self.Generation }

func (self *ClientSessionSnapshot) GetEventId() int64 { return self.EventId }

func (self *ClientSessionSnapshot) GetLoaded() bool { return self.Loaded }

func (self *ClientSessionSnapshot) GetLoading() bool { return self.Loading }

func (self *ClientSessionSnapshot) GetRefreshing() bool { return self.Refreshing }

func (self *ClientSessionSnapshot) GetSupported() bool { return self.Supported }

func (self *ClientSessionSnapshot) GetBulkAction() *ClientSessionAction { return self.BulkAction }

func (self *ClientSessionSnapshot) GetActions() *ClientSessionActionList { return self.Actions }

func (self *ClientSessionSnapshot) GetError() *ClientSessionError { return self.Error }

func (self *ClientSessionAction) GetStatus() string { return self.Status }
func (self *ClientSessionAction) GetState() string  { return self.State }
