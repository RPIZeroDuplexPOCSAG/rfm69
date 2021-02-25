package rfm69

// RXStream is the data structure for receiving data
type RXStream struct {
	ByteCounter int
	ByteStream	chan byte
	RSSI		chan int
	Cancel		bool
	Process		chan bool
}
