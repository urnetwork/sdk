





type DeviceRemoteAddress struct {
	Ip net.IP
	Port int
	// TODO allow using messages to communicate
}



type DeviceRemote struct {

	byJwt string
	
	address *DeviceRemoteAddress



}


GetAuthPublicKey() {

}

GetAuthPrivateKey() {
	
}





// attempt to connect in background
// when connected, run init sync (init keys, merge add/remove listeners, etc)
// 
// bi-directional grpc


// authentication is to sign the hello with the jwt


