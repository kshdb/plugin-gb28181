package gb28181

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/logrusorgru/aurora"
	"github.com/pion/rtp"
	"go.uber.org/zap"
	"m7s.live/engine/v4/util"
	"m7s.live/plugin/gb28181/v4/utils"

	"github.com/ghettovoice/gosip"
	"github.com/ghettovoice/gosip/log"
	"github.com/ghettovoice/gosip/sip"
)

var srv gosip.Server

type Server struct {
	Ignores    map[string]struct{}
	publishers util.Map[uint32, *GBPublisher]
	tcpPorts   PortManager
	udpPorts   PortManager
}

const MaxRegisterCount = 3

func FindChannel(deviceId string, channelId string) (c *Channel) {
	if v, ok := Devices.Load(deviceId); ok {
		d := v.(*Device)
		d.channelMutex.RLock()
		c = d.ChannelMap[channelId]
		d.channelMutex.RUnlock()
	}
	return
}

var levelMap = map[string]log.Level{
	"trace": log.TraceLevel,
	"debug": log.DebugLevel,
	"info":  log.InfoLevel,
	"warn":  log.WarnLevel,
	"error": log.ErrorLevel,
	"fatal": log.FatalLevel,
	"panic": log.PanicLevel,
}

func GetSipServer(transport string) gosip.Server {
	return srv
}

var sn = 0

func CreateRequest(exposedId string, Method sip.RequestMethod, recipient *sip.Address, netAddr string) (req sip.Request) {

	sn++

	callId := sip.CallID(utils.RandNumString(10))
	userAgent := sip.UserAgentHeader("Monibuca")
	cseq := sip.CSeq{
		SeqNo:      uint32(sn),
		MethodName: Method,
	}
	port := sip.Port(conf.SipPort)
	serverAddr := sip.Address{
		//DisplayName: sip.String{Str: d.config.Serial},
		Uri: &sip.SipUri{
			FUser: sip.String{Str: exposedId},
			FHost: conf.SipIP,
			FPort: &port,
		},
		Params: sip.NewParams().Add("tag", sip.String{Str: utils.RandNumString(9)}),
	}
	req = sip.NewRequest(
		"",
		Method,
		recipient.Uri,
		"SIP/2.0",
		[]sip.Header{
			serverAddr.AsFromHeader(),
			recipient.AsToHeader(),
			&callId,
			&userAgent,
			&cseq,
			serverAddr.AsContactHeader(),
		},
		"",
		nil,
	)

	req.SetTransport(conf.SipNetwork)
	req.SetDestination(netAddr)
	//fmt.Printf("构建请求参数:%s", *&req)
	// requestMsg.DestAdd, err2 = d.ResolveAddress(requestMsg)
	// if err2 != nil {
	// 	return nil
	// }
	//intranet ip , let's resolve it with public ip
	// var deviceIp, deviceSourceIP net.IP
	// switch addr := requestMsg.DestAdd.(type) {
	// case *net.UDPAddr:
	// 	deviceIp = addr.IP
	// case *net.TCPAddr:
	// 	deviceIp = addr.IP
	// }

	// switch addr2 := d.SourceAddr.(type) {
	// case *net.UDPAddr:
	// 	deviceSourceIP = addr2.IP
	// case *net.TCPAddr:
	// 	deviceSourceIP = addr2.IP
	// }
	// if deviceIp.IsPrivate() && !deviceSourceIP.IsPrivate() {
	// 	requestMsg.DestAdd = d.SourceAddr
	// }
	return
}
func RequestForResponse(transport string, request sip.Request,
	options ...gosip.RequestWithContextOption) (sip.Response, error) {
	return (GetSipServer(transport)).RequestWithContext(context.Background(), request, options...)
}

func (c *GB28181Config) startServer() {
	c.publishers.Init()
	addr := c.ListenAddr + ":" + strconv.Itoa(int(c.SipPort))

	logger := utils.NewZapLogger(plugin.Logger, "GB SIP Server", nil)
	logger.SetLevel(levelMap[c.LogLevel])
	// logger := log.NewDefaultLogrusLogger().WithPrefix("GB SIP Server")
	srvConf := gosip.ServerConfig{}
	if c.SipIP != "" {
		srvConf.Host = c.SipIP
	}
	srv = gosip.NewServer(srvConf, nil, nil, logger)
	srv.OnRequest(sip.REGISTER, c.OnRegister)
	srv.OnRequest(sip.MESSAGE, c.OnMessage)
	srv.OnRequest(sip.NOTIFY, c.OnNotify)
	srv.OnRequest(sip.BYE, c.OnBye)
	err := srv.Listen(strings.ToLower(c.SipNetwork), addr)
	if err != nil {
		plugin.Logger.Error("gb28181 server listen", zap.Error(err))
	} else {
		plugin.Info(fmt.Sprint(aurora.Green("Server gb28181 start at"), aurora.BrightBlue(addr)))
	}

	go c.startMediaServer()

	if c.Username != "" || c.Password != "" {
		go c.removeBanDevice()
	}
}

func (c *GB28181Config) startMediaServer() {
	if c.MediaNetwork == "tcp" {
		c.tcpPorts.Init(c.MediaPortMin, c.MediaPortMax)
		if !c.tcpPorts.Valid {
			c.listenMediaTCP()
		}
	} else {
		c.udpPorts.Init(c.MediaPortMin, c.MediaPortMax)
		if !c.udpPorts.Valid {
			c.listenMediaUDP()
		}
	}
}

func (c *GB28181Config) processTcpMediaConn(conn net.Conn) {
	var rtpPacket rtp.Packet
	reader := bufio.NewReader(conn)
	defer conn.Close()
	var err error
	dumpLen := make([]byte, 6)
	ps := make(util.Buffer, 1024)
	for err == nil {
		if _, err = io.ReadFull(reader, dumpLen[:2]); err != nil {
			return
		}
		ps.Relloc(int(binary.BigEndian.Uint16(dumpLen[:2])))
		if _, err = io.ReadFull(reader, ps); err != nil {
			return
		}
		if err := rtpPacket.Unmarshal(ps); err != nil {
			plugin.Error("gb28181 decode rtp error:", zap.Error(err))
		} else if publisher := c.publishers.Get(rtpPacket.SSRC); publisher != nil && publisher.Publisher.Err() == nil {
			publisher.writeDump(ps, dumpLen)
			publisher.PushPS(&rtpPacket)
		} else {
			plugin.Info("gb28181 publisher not found", zap.Uint32("ssrc", rtpPacket.SSRC))
		}
	}
}

func (c *GB28181Config) listenMediaTCP() {
	addr := ":" + strconv.Itoa(int(c.MediaPort))
	mediaAddr, _ := net.ResolveTCPAddr("tcp", addr)
	listen, err := net.ListenTCP("tcp", mediaAddr)

	if err != nil {
		plugin.Error("MediaServer listened　tcp err", zap.String("addr", addr), zap.Error(err))
		return
	}
	plugin.Sugar().Infof("MediaServer started tcp at %s", addr)
	defer listen.Close()
	defer plugin.Info("MediaServer stopped tcp at", zap.Uint16("port", c.MediaPort))

	for {
		conn, err := listen.Accept()
		if err != nil {
			plugin.Error("Accept err=", zap.Error(err))
		}
		go c.processTcpMediaConn(conn)
	}
}

func (c *GB28181Config) listenMediaUDP() {
	var rtpPacket rtp.Packet
	networkBuffer := 1048576

	addr := ":" + strconv.Itoa(int(c.MediaPort))
	mediaAddr, _ := net.ResolveUDPAddr("udp", addr)
	conn, err := net.ListenUDP("udp", mediaAddr)

	if err != nil {
		plugin.Error(" MediaServer started listening udp err", zap.String("addr", addr), zap.Error(err))
		return
	}
	bufUDP := make([]byte, networkBuffer)
	plugin.Sugar().Infof("MediaServer started at udp %s", addr)
	defer plugin.Sugar().Infof("MediaServer stopped at udp %s", addr)
	dumpLen := make([]byte, 6)
	for n, _, err := conn.ReadFromUDP(bufUDP); err == nil; n, _, err = conn.ReadFromUDP(bufUDP) {
		ps := bufUDP[:n]
		if err := rtpPacket.Unmarshal(ps); err != nil {
			plugin.Error("Decode rtp error:", zap.Error(err))
		}
		t := time.Now()
		if publisher := c.publishers.Get(rtpPacket.SSRC); publisher != nil && publisher.Publisher.Err() == nil {
			publisher.writeDump(ps, dumpLen)
			publisher.PushPS(&rtpPacket)
		}
		x := time.Since(t)
		if x > time.Millisecond {
			fmt.Println(x)
		}
	}
}

// func queryCatalog(config *transaction.Config) {
// 	t := time.NewTicker(time.Duration(config.CatalogInterval) * time.Second)
// 	for range t.C {
// 		Devices.Range(func(key, value interface{}) bool {
// 			device := value.(*Device)
// 			if time.Since(device.UpdateTime) > time.Duration(config.RegisterValidity)*time.Second {
// 				Devices.Delete(key)
// 			} else if device.Channels != nil {
// 				go device.Catalog()
// 			}
// 			return true
// 		})
// 	}
// }

func (c *GB28181Config) removeBanDevice() {
	t := time.NewTicker(c.RemoveBanInterval)
	for range t.C {
		DeviceRegisterCount.Range(func(key, value interface{}) bool {
			if value.(int) > MaxRegisterCount {
				DeviceRegisterCount.Delete(key)
			}
			return true
		})
	}
}
