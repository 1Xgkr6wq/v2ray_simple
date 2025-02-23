package proxy

import (
	"io"
	"net"
	"strings"
	"time"

	"github.com/e1732a364fed/v2ray_simple/netLayer"
	"github.com/xtaci/smux"
)

//配置文件格式
const (
	SimpleMode = iota
	StandardMode
	V2rayCompatibleMode
)

//规定，如果 proxy的server的handshake如果返回的是具有内层mux的连接，该连接要实现 MuxMarker 接口.
type MuxMarker interface {
	io.ReadWriteCloser
	IsMux()
}

//实现 MuxMarker
type MuxMarkerConn struct {
	netLayer.ReadWrapper
}

func (mh *MuxMarkerConn) IsMux() {}

// some client may 建立tcp连接后首先由客户端读服务端的数据？虽较少见但确实存在.
// Anyway firstpayload might not be read, and we should try to reduce this delay.
// 也有可能是有人用 nc 来测试，也会遇到这种读不到 firstpayload 的情况
const FirstPayloadTimeout = time.Millisecond * 100

// Client is used to dial a server.
// Because Server is "target agnostic",  Client's Handshake requires a target addr as param.
//
// A Client has all the data of all layers in its VSI model.
// Once a Client is fully defined, the flow of the data is fully defined.
type Client interface {
	ProxyCommon

	//Perform handshake when request is TCP。firstPayload 用于如 vless/trojan 这种 没有握手包的协议，可为空。
	Handshake(underlay net.Conn, firstPayload []byte, target netLayer.Addr) (wrappedConn io.ReadWriteCloser, err error)

	//Establish a channel and through this channel constantly request data for each UDP addr. target can be nil theoretically.
	EstablishUDPChannel(underlay net.Conn, target netLayer.Addr) (netLayer.MsgConn, error)

	IsUDP_MultiChannel() bool

	//get/listen a useable inner mux
	GetClientInnerMuxSession(wrc io.ReadWriteCloser) *smux.Session
	InnerMuxEstablished() bool
	CloseInnerMuxSession()
}

// Server is used for listening clients.
// Because Server is "target agnostic"，Handshake should return the target addr that the Client requested.
//
// A Server has all the data of all layers in its VSI model.
// Once a Server is fully defined, the flow of the data is fully defined.
type Server interface {
	ProxyCommon

	//ReadWriteCloser is for TCP request, net.PacketConn is for UDP request
	Handshake(underlay net.Conn) (net.Conn, netLayer.MsgConn, netLayer.Addr, error)

	//get/listen a useable inner mux
	GetServerInnerMuxSession(wlc io.ReadWriteCloser) *smux.Session
}

// FullName can fully represent the VSI model for a proxy.
// We think tcp/udp/kcp/raw_socket is FirstName，protocol of the proxy is LastName, and the rest is  MiddleName。
//
// An Example of a full name:  tcp+tls+ws+vless.
// 总之，类似【域名】的规则，只不过分隔符从 点号 变成了加号。
func GetFullName(pc ProxyCommon) string {
	if n := pc.Name(); n == "direct" {
		return n
	} else {

		return getFullNameBuilder(pc, n).String()
	}
}

func getFullNameBuilder(pc ProxyCommon, n string) *strings.Builder {

	var sb strings.Builder
	sb.WriteString(pc.Network())
	sb.WriteString(pc.MiddleName())
	sb.WriteString(n)

	if i, innerProxyName := pc.HasInnerMux(); i == 2 {
		sb.WriteString("+smux+")
		sb.WriteString(innerProxyName)

	}

	return &sb

}

// return GetFullName(pc) + "://" + pc.AddrStr()
func GetVSI_url(pc ProxyCommon) string {
	n := pc.Name()
	if n == "direct" {
		return "direct://"
	}
	sb := getFullNameBuilder(pc, n)
	sb.WriteString("://")
	sb.WriteString(pc.AddrStr())

	return sb.String()
}
