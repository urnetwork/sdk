//go:build !sdk_mobile_bind

// Getters for the session API's result types, for hosts that bind methods
// only: the C ABI exports a handle type's methods, never its fields. Gobind
// already binds every exported field as an accessor pair named after it (Java
// getX and setX, the Objective-C property x), so these getters would declare
// each Java method and JNI symbol twice and the Android bind would not compile.
// The gomobile view (sdk_mobile_bind) leaves them out; mobile apps read the
// fields through gobind's accessors, which keep the same Java names.
package sdk

func (self *SessionLastUsed) GetUnixTime() int64 { return self.UnixTime }

func (self *SessionLastUsed) GetCity() string { return self.City }

func (self *SessionLastUsed) GetRegion() string { return self.Region }

func (self *SessionLastUsed) GetCountry() string { return self.Country }

func (self *SessionLastUsed) GetCountryCode() string { return self.CountryCode }

func (self *SessionLastUsed) GetDeviceType() string { return self.DeviceType }

func (self *SessionLastUsed) GetAppVersion() string { return self.AppVersion }

func (self *NetworkSessionInfo) GetSessionId() *Id { return self.SessionId }

func (self *NetworkSessionInfo) GetCurrent() bool { return self.Current }

func (self *NetworkSessionInfo) GetKind() string { return self.Kind }

func (self *NetworkSessionInfo) GetCreateTime() *Time { return self.CreateTime }

func (self *NetworkSessionInfo) GetLastMintTime() *Time { return self.LastMintTime }

func (self *NetworkSessionInfo) GetTokenExpireTime() *Time { return self.TokenExpireTime }

func (self *NetworkSessionInfo) GetAcceptUntil() *Time { return self.AcceptUntil }

func (self *NetworkSessionInfo) GetOriginSessionId() *Id { return self.OriginSessionId }

func (self *NetworkSessionInfo) GetLastUsed() *SessionLastUsed { return self.LastUsed }

func (self *NetworkSessionsRevision) GetGeneration() string { return self.Generation }

func (self *NetworkSessionsRevision) GetEventId() int64 { return self.EventId }

func (self *NetworkSessionsResult) GetSessions() *NetworkSessionInfoList { return self.Sessions }

func (self *NetworkSessionsResult) GetGeneration() string { return self.Generation }

func (self *NetworkSessionsResult) GetEventId() int64 { return self.EventId }

func (self *NetworkSessionsResult) GetCurrentSessionId() *Id { return self.CurrentSessionId }

func (self *NetworkSessionsResult) GetLegacyCoverage() string { return self.LegacyCoverage }

func (self *SessionOperationResult) GetOperationId() *Id { return self.OperationId }

func (self *SessionOperationResult) GetStatus() string { return self.Status }

func (self *SessionOperationResult) GetState() string { return self.State }

func (self *SessionOperationResult) GetSessionId() *Id { return self.SessionId }

func (self *SessionOperationResult) GetKeptSessionId() *Id { return self.KeptSessionId }

func (self *SessionOperationResult) GetRevokedCount() int { return self.RevokedCount }

func (self *SessionOperationResult) GetCleanupPending() bool { return self.CleanupPending }

func (self *SessionOperationResult) GetGeneration() string { return self.Generation }

func (self *SessionOperationResult) GetEventId() int64 { return self.EventId }

func (self *SessionSignOutResult) GetOperationId() *Id          { return self.OperationId }
func (self *SessionSignOutResult) GetRevocationConfirmed() bool { return self.RevocationConfirmed }
func (self *SessionSignOutResult) GetCredentialCleared() bool   { return self.CredentialCleared }
