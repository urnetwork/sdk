package sdk

// Hands one complete message to the browser websocket reader without parking
// the shared JavaScript callback. Refusal is terminal for that RPC generation;
// continuing after dropping a reliable message would corrupt the byte stream.
func offerDeviceRpcReceive(
	done <-chan struct{},
	receive chan<- []byte,
	message []byte,
) bool {
	select {
	case <-done:
		return false
	default:
	}

	select {
	case <-done:
		return false
	case receive <- message:
		return true
	default:
		return false
	}
}
