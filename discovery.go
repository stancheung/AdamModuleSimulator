package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
)

// Advantech ADAM/APAX .NET Utility discovery + ASCII channel emulation.
//
//   - UDP 5048: 'MADA' discovery broadcast -> NetConfig (type 16) and
//     DeviceInfo (type 0) replies, so the utility lists the module.
//   - UDP 1025: ASCII command channel (Apax5070-style path).
//   - The Modbus TCP ASCII path (FC16 write to reg 10000 / FC3 read reply)
//     lives in adam.go; both paths share handleASCII here.
//
// Wire layout verified against the decompiled client (Advantech.Adam.dll,
// AdamSocket.GetAdamInformation / Configuration.GetModuleName).

// moduleIdentity is what the simulator advertises as.
type moduleIdentity struct {
	Name     string // e.g. "6050" -> shown as ADAM-6050
	Desc     string
	DevID    [2]byte // 0x60,0x50 = ADAM-6050
	FwVer    string
	MAC      [6]byte
	IP       string
	Subnet   string
	Gateway  string
	Password string // 8 chars
	Port     int    // advertised Modbus TCP port
}

func xorPW(s string) []byte {
	b := []byte(s + "        ")
	out := make([]byte, 8)
	for i := 0; i < 8; i++ {
		out[i] = b[i] ^ 0x3F
	}
	return out
}

func (id *moduleIdentity) basePacket(b52, b53 byte) []byte {
	p := make([]byte, 120)
	copy(p[0:4], "MADA")
	p[7] = 0x83
	p[8] = 1
	p[10] = 0x50
	p[52] = b52
	p[53] = b53
	p[10] = id.DevID[0]
	p[11] = id.DevID[1]
	p[12] = 0 // SetMac skips [12]
	copy(p[13:16], id.MAC[0:3])
	copy(p[17:20], id.MAC[3:6])
	return p
}

func (id *moduleIdentity) netConfigReply() []byte {
	p := id.basePacket(0x80, 16)
	copy(p[20:28], xorPW(id.Password))
	p[66] = 0 // ethernet mode
	p[67] = 0
	p[74] = 0 // host idle off
	copy(p[76:78], "PN")
	binary.BigEndian.PutUint16(p[78:80], uint16(id.Port))
	copy(p[80:84], net.ParseIP(id.IP).To4())
	copy(p[84:88], net.ParseIP(id.Subnet).To4())
	copy(p[88:92], net.ParseIP(id.Gateway).To4())
	return p[:92]
}

func (id *moduleIdentity) deviceInfoReply() []byte {
	p := id.basePacket(0x80, 0)
	// FwVer is BINARY at [56..60] (major, minor, ...), not ASCII:
	// the utility gates on FwVer[1] >= 5 to call RefreshModbusTable()
	// (flexible-modbus XML download). Advertise 4.02 to skip that path.
	p[56] = 0
	p[57] = 4
	p[58] = 2
	p[59] = 0
	p[60] = 0 // hardware type: wired
	p[61] = 1
	p[70] = 0 // module status ok
	p[71] = 0
	p[75] = byte(len(id.Name))
	p[83] = byte(len(id.Desc))
	copy(p[88:], id.Name)
	copy(p[88+len(id.Name):], id.Desc)
	return p[:88+len(id.Name)+len(id.Desc)]
}

// startDiscovery runs the UDP 5048 discovery responder and the UDP 1025
// ASCII channel. Bind failures are logged, not fatal (web panel keeps up).
func (adamServer *AdamServer) startDiscovery(id *moduleIdentity) {
	go adamServer.discoveryLoop(id)
	go adamServer.udpASCIILoop(id)
}

func (adamServer *AdamServer) discoveryLoop(id *moduleIdentity) {
	addr := &net.UDPAddr{Port: 5048, IP: net.IPv4zero}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		fmt.Printf("[-] Cannot listen on UDP 5048: %v\n", err)
		return
	}
	fmt.Println("[+] UDP discovery on 5048")
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			fmt.Printf("[-] discovery read: %v\n", err)
			continue
		}
		if n < 54 || string(buf[0:4]) != "MADA" {
			continue
		}
		switch buf[53] {
		case 16:
			conn.WriteToUDP(id.netConfigReply(), from)
			fmt.Printf("[d] NetConfig -> %s\n", from)
		case 0:
			conn.WriteToUDP(id.deviceInfoReply(), from)
			fmt.Printf("[d] DeviceInfo -> %s\n", from)
		}
	}
}

func (adamServer *AdamServer) udpASCIILoop(id *moduleIdentity) {
	addr := &net.UDPAddr{Port: 1025, IP: net.IPv4zero}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		fmt.Printf("[-] Cannot listen on UDP 1025: %v\n", err)
		return
	}
	fmt.Println("[+] UDP ASCII on 1025")
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			fmt.Printf("[-] ascii udp read: %v\n", err)
			continue
		}
		reply := handleASCII(id, string(buf[:n]))
		conn.WriteToUDP([]byte(reply), from)
	}
}

// handleASCII implements the ADAM ASCII command set the utility uses.
// Replies are '!'+addr+DATA+'\r'; every $-query reply needs total len > 4
// or the utility reports "Connect module failed! Please check subnet mask
// or IP address."
func handleASCII(id *moduleIdentity, cmd string) string {
	// '$AA PWx <8 obfuscated bytes>\r' is FIXED LENGTH 14: obfuscated bytes
	// can contain \r or NUL, so slice — never split/strip.
	if strings.HasPrefix(cmd, "$") {
		if len(cmd) > 14 {
			cmd = cmd[:14]
		}
	} else {
		if i := strings.IndexByte(cmd, '\r'); i >= 0 {
			cmd = cmd[:i]
		}
		cmd = strings.TrimRight(cmd, "\x00")
	}
	fmt.Printf("[cmd] %q\n", cmd)
	if cmd == "" || strings.IndexByte("$%#@", cmd[0]) < 0 {
		return "?"
	}
	addr := ""
	if len(cmd) >= 3 {
		addr = cmd[1:3]
	}
	if len(cmd) >= 14 && cmd[0] == '$' {
		switch cmd[3:6] {
		case "PW0": // password check
			if cmd[6:14] == string(xorPW(id.Password)) {
				return "!" + addr + "\r"
			}
			return "?" + addr + "\r"
		case "PW1":
			return "!" + addr + "\r"
		case "PW2": // read password (some models)
			return "!" + addr + string(xorPW(id.Password)) + "\r"
		}
	}
	if cmd[0] == '$' {
		// Modbus word-aligns odd-length payloads with NUL -> strip \r and \x00.
		sub := strings.TrimRight(cmd[3:], "\r\x00")
		switch sub {
		case "M": // module name
			return "!" + addr + id.Name + "\r"
		case "F", "FMPV": // firmware version — "4.02" so ChkFwSup's
			// Substring(1,4)/Substring(2,4) parses both fail -> num=0 ->
			// the utility skips the >=3.01 access-control and XML paths.
			return "!" + addr + id.FwVer + "\r"
		case "T": // slot/module type list
			return "!" + addr + "DIDIDO00\r"
		case "NC": // net config: ip;mask;gw
			return "!" + addr + id.IP + ";" + id.Subnet + ";" + id.Gateway + "\r"
		case "SEFFFF": // access control status: '1' at reply index 3 = allow
			return "!" + addr + "1\r"
		case "PD": // flexible-modbus table ID: 8 hex chars at reply index 3
			return "!" + addr + "00000000\r"
		}
	}
	return "!" + addr + "\r"
}

// detectLocalIP returns the first up, non-loopback IPv4 address.
func detectLocalIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip4 := ip.To4(); ip4 != nil && !ip4.IsLoopback() {
				return ip4.String()
			}
		}
	}
	return "127.0.0.1"
}

// detectMAC returns the hardware address of the interface owning ip, or a
// synthetic locally-administered MAC.
func detectMAC(ip string) [6]byte {
	var mac [6]byte
	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, a := range addrs {
				ipNet, ok := a.(*net.IPNet)
				if ok && ipNet.IP.Equal(net.ParseIP(ip)) && len(iface.HardwareAddr) == 6 {
					copy(mac[:], iface.HardwareAddr)
					return mac
				}
			}
		}
	}
	// synthetic: locally administered, unicast
	octets := net.ParseIP(ip).To4()
	if octets == nil {
		octets = []byte{0, 0, 0, 1}
	}
	mac = [6]byte{0x02, 0x00, octets[1], octets[2], octets[3], 0x01}
	return mac
}

func gatewayFor(ip string) string {
	octets := strings.Split(ip, ".")
	if len(octets) != 4 {
		return "127.0.0.1"
	}
	return octets[0] + "." + octets[1] + "." + octets[2] + ".1"
}
