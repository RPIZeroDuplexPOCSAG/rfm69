package rfm69

import (
	"log"
	"fmt"
	"github.com/davecheney/gpio"
)

// Send data
func (r *Device) Send(d *Data) {
	if r.TXFreq == 0 {
		return
	}
	if (r._invert) {
		for i := 0; i < len(d.Data); i++ {
			d.Data[i] = 255 - d.Data[i]
		}
	}
	r.tx <- d
	log.Println("sending", len(d.Data));
}

func min(a, b int) int {
    if a <= b {
        return a
    }
    return b
}
func (r *Device) Loop() {
	log.Println("entering loop");
	irq := make(chan int)
	r.gpio.BeginWatch(gpio.EdgeRising, func() {
		irq <- 1
	})
	defer r.gpio.EndWatch()

	err := r.SetMode(RF_OPMODE_RECEIVER)
	if err != nil {
		log.Fatal(err)
	}
	defer r.SetMode(RF_OPMODE_STANDBY)

	for {
		select {
		case dataToTransmit := <-r.tx:
			log.Println("going to transmit");
			r.PrepareTX();
			// TODO: can send?
			r.readWriteReg(REG_PACKETCONFIG2, 0xFB, RF_PACKET2_RXRESTART) // avoid RX deadlocks
			err = r.SetModeAndWait(RF_OPMODE_STANDBY)
			if err != nil {
				log.Fatal(err)
			}
			err = r.writeReg(REG_DIOMAPPING1, RF_DIOMAPPING1_DIO0_00)
			if err != nil {
				log.Fatal(err)
			}
			transferLimit := 16
			transferLength := len(dataToTransmit.Data)
			batch1 := dataToTransmit.Data[0:min(0+transferLimit, transferLength)]

			if err = r.WriteFifoData(batch1); err != nil {
				log.Fatal(err)
			}
			err = r.SetMode(RF_OPMODE_TRANSMITTER)
			if err != nil {
				log.Fatal(err)
			}
			for i := 1; i < transferLength; i += transferLimit {
				batchN := dataToTransmit.Data[i:min(i+transferLimit, transferLength)]
				if err = r.WriteFifoData(batchN); err != nil {
					log.Fatal(err)
				}
				for {
					reg, err := r.readReg(REG_IRQFLAGS2)
					if err != nil {
						log.Fatal(err)
						panic(err)
						break
					}
					//fmt.Printf("% d", reg & RF_IRQFLAGS2_FIFOLEVEL);
					if reg & RF_IRQFLAGS2_FIFOLEVEL < 16 {
						break
					}
				}
				//fmt.Printf("% d", i)
				//log.Println("Fifowrite2");
			}
			for {
				reg, err := r.readReg(REG_IRQFLAGS2)
				if err != nil {
					log.Fatal(err)
					panic(err)
					break
				}
				if reg&RF_IRQFLAGS2_PACKETSENT != 0 {
					break
				}
			}

			//<-irq
			//log.Println("ieq");

			err = r.SetModeAndWait(RF_OPMODE_STANDBY)
			if err != nil {
				log.Fatal(err)
			}
			err = r.writeReg(REG_DIOMAPPING1, RF_DIOMAPPING1_DIO0_01)
			if err != nil {
				log.Fatal(err)
			}
			err = r.SetMode(RF_OPMODE_RECEIVER)
			if err != nil {
				log.Fatal(err)
			}
			log.Println("tx done");
			r.PrepareRX();

			break
		case <-r.quit:
			r.quit <- true
			return
		default:
			if r.RXFreq == 0 {
				continue
			}
			if r.mode != RF_OPMODE_RECEIVER {
				continue
			}
			flags, err := r.readReg(REG_IRQFLAGS2)
			if err != nil {
				log.Fatal(err)
			}
			if flags & RF_IRQFLAGS2_FIFONOTEMPTY == 0 { // Entry for [1] * if 0/Clear SKIP
				continue
			}
			// fmt.Printf("% 08b\n", flags)
			fmt.Println("RX start")
			if err = r.EnterRX(); err != nil {
				log.Fatal(err)
			}
			err = r.SetMode(RF_OPMODE_RECEIVER)
			if err != nil {
				log.Fatal(err)
			}
		}
	}
}

func (r *Device) EnterRX() (error) {
	/*
	1) Start reading bytes from the FIFO when FifoNotEmpty or FifoThreshold becomes set.
	2) Suspend reading from the FIFO if FifoNotEmpty clears before all bytes of the message have been read
	3) Continue to step 1 until PayloadReady or CrcOk fires
	4) Read all remaining bytes from the FIFO either in Rx or Sleep/Standby mode
	*/
	stream := &RXStream{
		ByteCounter: 0,
		ByteStream: make(chan byte, 1024e2), // 10 KiB Bufer
		RSSI: make(chan int),
		Cancel: false,
		Process: make(chan bool),
	}
	if r.OnReceive != nil {
		go r.OnReceive(stream)
	}

	for {
		if stream.Cancel {
			fmt.Println("cancel request")
			break
		}
		for {
			if stream.Cancel {
				fmt.Println("cancel request")
				break
			}
			flags, err := r.readReg(REG_IRQFLAGS2)
			if err != nil {
				return err
			}
			//fmt.Printf("% 08b\n", flags)
			if flags & RF_IRQFLAGS2_FIFONOTEMPTY == 0 { // Check if we need to Suspend for [2] if 0/Clear BREAK
				//fmt.Println("FIFO Not empty is cleared")
				break
			}
			if flags & RF_IRQFLAGS2_PAYLOADREADY != 0 { // Check if we need to STOP because of [3] if 1/Set BREAK
				//fmt.Println("PayloadReady set")
				break
			}
			byte1, err := r.readReg(0x00)
			if err != nil {
				return err
			}
			stream.ByteStream <- byte1
			stream.ByteCounter++

			if (stream.ByteCounter % 4 == 0) {
				rssi, err := r.readRSSI(false)
				if err != nil {
					return err
				}
				stream.RSSI <- rssi
			}
		}
		if stream.Cancel {
			fmt.Println("cancel request")
			break
		}
		flags, err := r.readReg(REG_IRQFLAGS2)
		if err != nil {
			return err
		}
		//fmt.Printf("% 08b\n", flags)
		if flags & RF_IRQFLAGS2_PAYLOADREADY != 0 { // Check if we need to STOP because of [3] if 1/Set BREAK
			fmt.Println("PayloadReady set")
			break
		}
		if (stream.ByteCounter % 4 == 0) {
			rssi, err := r.readRSSI(false)
			if err != nil {
				return err
			}
			stream.RSSI <- rssi
		}
	}
	// reading the final content of the FIFO
	flags, err := r.readReg(REG_IRQFLAGS2)
	if err != nil {
		return err
	}
	if flags & RF_IRQFLAGS2_FIFONOTEMPTY != 0 {
		byte1, err := r.readReg(0x00)
		if err != nil {
			return err
		}
		stream.ByteStream <- byte1
		stream.ByteCounter++
	}

	// ... done
	fmt.Println("RX end")
	stream.Process <- true
	//
	defer r.SetMode(RF_OPMODE_STANDBY)
	fmt.Println("RXMode end")
	return nil
}