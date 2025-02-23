package v2ray_simple

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"golang.org/x/net/http2"

	"github.com/e1732a364fed/v2ray_simple/advLayer"
	"github.com/e1732a364fed/v2ray_simple/httpLayer"
	"github.com/e1732a364fed/v2ray_simple/netLayer"
	"github.com/e1732a364fed/v2ray_simple/proxy"
	"github.com/e1732a364fed/v2ray_simple/tlsLayer"
	"github.com/e1732a364fed/v2ray_simple/utils"

	_ "github.com/e1732a364fed/v2ray_simple/advLayer/ws"

	_ "github.com/e1732a364fed/v2ray_simple/proxy/dokodemo"
	_ "github.com/e1732a364fed/v2ray_simple/proxy/http"
	_ "github.com/e1732a364fed/v2ray_simple/proxy/simplesocks"
	_ "github.com/e1732a364fed/v2ray_simple/proxy/socks5"
	_ "github.com/e1732a364fed/v2ray_simple/proxy/trojan"
	_ "github.com/e1732a364fed/v2ray_simple/proxy/vless"
)

//statistics
var (
	ActiveConnectionCount      int32
	AllDownloadBytesSinceStart uint64
	AllUploadBytesSinceStart   uint64
)

var (
	DirectClient, _, _ = proxy.ClientFromURL(proxy.DirectName + "://")
)

//用于回落到h2c
var (
	h2c_transport = &http2.Transport{
		DialTLS: func(n, a string, cfg *tls.Config) (net.Conn, error) {
			return net.Dial(n, a)
		},
		AllowHTTP: true,
	}

	h2c_PROXYprotocolAddrMap       = make(map[string]*http2.Transport)
	h2c_PROXYprotocolAddrMap_mutex sync.RWMutex
)

// ListenSer 函数 是本包 最重要的函数。可以 直接使用 本函数 来手动开启新的 自定义的 转发流程。
// 监听 inServer, 然后试图转发到一个 proxy.Client。如果env没给出，则会转发到 defaultOutClient。
// 若 env 不为 nil, 则会 进行分流或回落。具有env的情况下，可能会转发到 非 defaultOutClient 的其他 proxy.Client.
//
// Use cases: refer to tcp_test.go, udp_test.go or cmd/verysimple.
//
// non-blocking. closer used to stop listening. It means listening failed if closer == nil,
func ListenSer(inServer proxy.Server, defaultOutClient proxy.Client, env *proxy.RoutingEnv) (closer io.Closer) {

	var handleHere bool
	advs := inServer.GetAdvServer()

	if advs != nil {
		handleHere = advs.IsSuper() && advs.IsMux()
	}

	if handleHere {
		//如果像quic一样自行处理传输层至tls层之间的部分，则我们跳过 handleNewIncomeConnection 函数
		// 拿到连接后直接调用 handshakeInserver_and_passToOutClient

		superSer := advs.(advLayer.SuperMuxServer)

		var newConnChan chan net.Conn

		newConnChan, closer = superSer.StartListen()
		if newConnChan == nil {
			utils.Error("superSer.StartListen failed ")
			return
		}

		go func() {
			for {
				newConn, ok := <-newConnChan
				if !ok {
					if ce := utils.CanLogErr("Read chan from Super AdvLayer closed"); ce != nil {
						ce.Write(zap.String("advLayer", inServer.AdvancedLayer()))
					}

					if closer != nil {
						closer.Close()
					}

					return
				}

				iics := incomingInserverConnState{
					wrappedConn:   newConn,
					inServer:      inServer,
					defaultClient: defaultOutClient,
					RoutingEnv:    env,
				}

				go handshakeInserver_and_passToOutClient(iics)
			}

		}()

		if ce := utils.CanLogInfo("Listening Super AdvLayer"); ce != nil {

			ce.Write(
				zap.String("protocol", proxy.GetFullName(inServer)),
				zap.String("addr", inServer.AddrStr()),
			)
		}
		return
	}

	var err error

	closer, err = netLayer.ListenAndAccept(
		inServer.Network(),
		inServer.AddrStr(),
		inServer.GetSockopt(),
		func(conn net.Conn) {
			handleNewIncomeConnection(inServer, defaultOutClient, conn, env)
		},
	)

	if err == nil {
		if ce := utils.CanLogInfo("Listening"); ce != nil {

			ce.Write(
				zap.String("protocol", proxy.GetFullName(inServer)),
				zap.String("addr", inServer.AddrStr()),
			)
		}

	} else {
		if err != nil {
			if ce := utils.CanLogErr("ListenSer failed"); ce != nil {
				ce.Write(
					zap.String("addr", inServer.AddrStr()),
					zap.Error(err),
				)
			}

		}
	}
	return
}

// handleNewIncomeConnection 会处理 网络层至高级层的数据，
// 然后将代理层的处理发往 handshakeInserver_and_passToOutClient 函数。
//
// 在 listenSer 中被调用。
func handleNewIncomeConnection(inServer proxy.Server, defaultClientForThis proxy.Client, thisLocalConnectionInstance net.Conn, env *proxy.RoutingEnv) {

	iics := incomingInserverConnState{
		baseLocalConn: thisLocalConnectionInstance,
		inServer:      inServer,
		defaultClient: defaultClientForThis,
		RoutingEnv:    env,
	}

	iics.isTlsLazyServerEnd = Tls_lazy_encrypt && canLazyEncryptServer(inServer)

	wrappedConn := thisLocalConnectionInstance

	if ce := utils.CanLogInfo("New Accepted Conn"); ce != nil {

		addrstr := wrappedConn.RemoteAddr().String()
		ce.Write(
			zap.String("from", addrstr),
			zap.String("handler", proxy.GetVSI_url(inServer)),
		)

		iics.cachedRemoteAddr = addrstr
	}

	////////////////////////////// tls层 /////////////////////////////////////

	//此时，baseLocalConn里面 正常情况下, 服务端看到的是 客户端的golang的tls 拨号发出的 tls数据
	// 客户端看到的是 socks5的数据， 我们首先就是要看看socks5里的数据是不是tls，而socks5自然 IsUseTLS 是false

	// 如果是服务端的话，那就是 inServer.IsUseTLS == true, 此时，我们正常握手，然后我们需要判断的是它承载的数据

	// 每次tls试图从 原始连接 读取内容时，都会附带把原始数据写入到 这个 Recorder中

	if inServer.IsUseTLS() {

		if iics.isTlsLazyServerEnd {
			iics.inServerTlsRawReadRecorder = tlsLayer.NewRecorder()

			iics.inServerTlsRawReadRecorder.StopRecord() //先不记录，因为一开始是我们自己的tls握手包，没有意义
			teeConn := tlsLayer.NewTeeConn(wrappedConn, iics.inServerTlsRawReadRecorder)

			wrappedConn = teeConn
		}

		tlsConn, err := inServer.GetTLS_Server().Handshake(wrappedConn)
		if err != nil {

			if ce := utils.CanLogErr("tls handshake failed"); ce != nil {
				ce.Write(
					zap.String("inServer", inServer.AddrStr()),
					zap.Error(err),
				)

			}
			wrappedConn.Close()
			return
		}

		if iics.isTlsLazyServerEnd {
			//此时已经握手完毕，可以记录了
			iics.inServerTlsRawReadRecorder.StartRecord()
		}

		iics.inServerTlsConn = tlsConn
		wrappedConn = tlsConn

	}

	adv := inServer.AdvancedLayer()
	advSer := inServer.GetAdvServer()

	////////////////////////////// header 层 /////////////////////////////////////

	if header := inServer.HasHeader(); header != nil {

		//websocket 可以自行处理header, 不需要额外http包装
		if !(advSer != nil && advSer.CanHandleHeaders()) {
			wrappedConn = &httpLayer.HeaderConn{
				Conn:        wrappedConn,
				H:           header,
				IsServerEnd: true,
			}

		}

	}

	////////////////////////////// 高级层 /////////////////////////////////////

	if adv != "" {
		//这里分 super, mux 和 default 三种情况，实际上对应了 quic，grpc 和ws

		switch {
		case advSer.IsSuper():
		//Super 根本没调用 handleNewIncomeConnection 函数，所以不在此处理
		// 详见 ListenSer

		case advSer.IsMux(): //grpc

			muxSer := advSer.(advLayer.MuxServer)

			newConnChan := make(chan net.Conn, 10)
			fallbackChan := make(chan httpLayer.FallbackMeta, 10)
			muxSer.StartHandle(wrappedConn, newConnChan, fallbackChan)

			go func() {
				for {
					fallbackMeta, ok := <-fallbackChan

					if !ok {
						if ce := utils.CanLogWarn("grpc read fallbackChan not ok"); ce != nil {
							ce.Write()
						}
						return
					}

					newiics := iics

					newiics.theRequestPath = fallbackMeta.Path
					newiics.theFallbackFirstBuffer = fallbackMeta.H1RequestBuf
					newiics.wrappedConn = fallbackMeta.Conn
					newiics.isFallbackH2 = fallbackMeta.IsH2
					newiics.fallbackH2Request = fallbackMeta.H2Request

					passToOutClient(newiics, true, nil, nil, netLayer.Addr{})
				}
			}()

			for {

				newGConn, ok := <-newConnChan

				if !ok {
					if ce := utils.CanLogWarn("grpc getNewSubConn not ok"); ce != nil {
						ce.Write()
					}

					iics.baseLocalConn.Close()
					return
				}

				iics.wrappedConn = newGConn

				go handshakeInserver_and_passToOutClient(iics)
			}

		default: //ws
			singleSer := advSer.(advLayer.SingleServer)

			wsConn, err := singleSer.Handshake(wrappedConn)

			if errors.Is(err, httpLayer.ErrShouldFallback) {

				meta := wsConn.(httpLayer.FallbackMeta)

				iics.theRequestPath = meta.Path
				iics.theFallbackFirstBuffer = meta.H1RequestBuf
				iics.wrappedConn = meta.Conn

				if ce := utils.CanLogDebug("Single AdvLayer Check failed, will fallback."); ce != nil {

					ce.Write(
						zap.String("handler", inServer.AddrStr()),
						zap.String("advLayer", adv),
						zap.String("validPath", advSer.GetPath()),
						zap.String("gotMethod", meta.Method),
						zap.String("gotPath", meta.Path),
					)
				}

				passToOutClient(iics, true, nil, nil, netLayer.Addr{})
				return

			} else if err != nil {
				if ce := utils.CanLogErr("InServer Single AdvLayer handshake failed"); ce != nil {

					ce.Write(
						zap.String("handler", inServer.AddrStr()),
						zap.String("advLayer", adv),
						zap.Error(err),
					)
				}

				wrappedConn.Close()
				return

			} else {
				wrappedConn = wsConn

			}
		} // switch adv

	} //if adv !=""

	iics.wrappedConn = wrappedConn

	handshakeInserver_and_passToOutClient(iics)
}

//被 handshakeInserver_and_passToOutClient 调用
func handshakeInserver(iics *incomingInserverConnState) (wlc net.Conn, udp_wlc netLayer.MsgConn, targetAddr netLayer.Addr, err error) {
	inServer := iics.inServer
	if inServer == nil {
		err = utils.ErrInErr{ErrDesc: "handshakeInserver failed, nil inServer", ErrDetail: utils.ErrNilParameter}
		return
	}

	wlc, udp_wlc, targetAddr, err = inServer.Handshake(iics.wrappedConn)

	if err == nil {
		if udp_wlc != nil && inServer.Name() == "socks5" {
			// socks5的 udp associate返回的是 clientFutureAddr, 而不是实际客户的第一个请求.
			//所以我们要读一次才能进行下一步。

			firstSocks5RequestData, firstSocks5RequestAddr, err2 := udp_wlc.ReadMsgFrom()
			if err2 != nil {
				if ce := utils.CanLogWarn("failed in socks5 read"); ce != nil {
					ce.Write(
						zap.String("handler", inServer.AddrStr()),
						zap.Error(err2),
					)
				}
				err = err2
				return
			}

			iics.theFallbackFirstBuffer = bytes.NewBuffer(firstSocks5RequestData)

			targetAddr = firstSocks5RequestAddr
		}

		////////////////////////////// 内层mux阶段 /////////////////////////////////////

		if muxInt, innerProxyName := inServer.HasInnerMux(); muxInt > 0 {
			if mh, ok := wlc.(proxy.MuxMarker); ok {

				innerSerConf := proxy.ListenConf{
					CommonConf: proxy.CommonConf{
						Protocol: innerProxyName,
					},
				}

				innerSer, err2 := proxy.NewServer(&innerSerConf)
				if err2 != nil {
					if ce := utils.CanLogDebug("mux inner proxy server creation failed"); ce != nil {
						ce.Write(zap.Error(err))
					}
					err = err2
					return
				}

				session := inServer.GetServerInnerMuxSession(mh)

				if session == nil {
					err = utils.ErrFailed
					return
				}

				//内层mux要对每一个子连接单独进行 子代理协议握手 以及 outClient的拨号。

				go func() {

					for {
						utils.Debug("inServer try accept smux stream ")

						stream, err := session.AcceptStream()
						if err != nil {
							if ce := utils.CanLogDebug("mux inServer try accept stream failed"); ce != nil {
								ce.Write(zap.Error(err))
							}

							session.Close()
							return
						}

						go func() {
							if ce := utils.CanLogDebug("inServer got mux stream"); ce != nil {
								ce.Write(zap.String("innerProxyName", innerProxyName))
							}

							wlc1, udp_wlc1, targetAddr1, err1 := innerSer.Handshake(stream)

							if err1 != nil {
								if ce := utils.CanLogDebug("inServer mux inner proxy handshake failed"); ce != nil {
									ce.Write(zap.Error(err1))
								}
								newiics := *iics

								if !newiics.extractFirstBufFromErr(err1) {
									return
								}
								passToOutClient(newiics, true, wlc1, udp_wlc1, targetAddr1)

							} else {

								if ce := utils.CanLogDebug("inServer mux stream handshake ok"); ce != nil {
									ce.Write(zap.String("targetAddr1", targetAddr1.String()))
								}

								passToOutClient(*iics, false, wlc1, udp_wlc1, targetAddr1)

							}

						}()

					}
				}()

				err = utils.ErrHandled
				return
			}
		}

	}

	return
}

// 本函数 处理inServer的代理层数据，并在试图处理 分流和回落后，将流量导向目标，并开始Copy。
// iics 不使用指针, 因为iics不能公用，因为 在多路复用时 iics.wrappedConn 是会变化的。
//
//被 handleNewIncomeConnection 调用。
func handshakeInserver_and_passToOutClient(iics incomingInserverConnState) {

	wlc, udp_wlc, targetAddr, err := handshakeInserver(&iics)

	switch err {
	case nil:
		passToOutClient(iics, false, wlc, udp_wlc, targetAddr)

	case utils.ErrHandled:
		return

	default:
		if !iics.extractFirstBufFromErr(err) {
			return
		}

		passToOutClient(iics, true, nil, nil, netLayer.Addr{})
	}

}

//被 handshakeInserver_and_passToOutClient 和 handshakeInserver 的innerMux部分 调用。
// 会调用 dialClient_andRelay. 若isfallback为true，传入的 wlc 和 udp_wlc 必须为nil，targetAddr必须为空值。
func passToOutClient(iics incomingInserverConnState, isfallback bool, wlc net.Conn, udp_wlc netLayer.MsgConn, targetAddr netLayer.Addr) {

	////////////////////////////// 回落阶段 /////////////////////////////////////

	if isfallback {

		fallbackTargetAddr, fbResult := checkfallback(iics)
		if fbResult >= 0 {
			targetAddr = fallbackTargetAddr
			wlc = iics.wrappedConn

			if iics.isFallbackH2 {
				//h2 的fallback 非常特殊，要单独处理. 下面进行h2c拨号并向真实h2c服务器发起请求。

				rq := iics.fallbackH2Request
				rq.Host = targetAddr.Name
				targetAddrStr := targetAddr.String()
				urlStr := "https://" + targetAddrStr + iics.theRequestPath
				url, _ := url.Parse(urlStr)
				rq.URL = url

				var transport *http2.Transport

				if fbResult == 0 {
					transport = h2c_transport

				} else if fbResult > 0 {

					h2c_PROXYprotocolAddrMap_mutex.RLock()
					transport = h2c_PROXYprotocolAddrMap[targetAddrStr]
					h2c_PROXYprotocolAddrMap_mutex.RUnlock()

					if transport == nil {
						transport = &http2.Transport{
							DialTLS: func(n, a string, cfg *tls.Config) (net.Conn, error) {
								conn, e := net.Dial(n, a)
								netLayer.WritePROXYprotocol(fbResult, wlc, conn)
								return conn, e
							},
							AllowHTTP: true,
						}

						h2c_PROXYprotocolAddrMap_mutex.Lock()
						h2c_PROXYprotocolAddrMap[targetAddrStr] = transport
						h2c_PROXYprotocolAddrMap_mutex.Unlock()

					}

				}

				rsp, err := transport.RoundTrip(iics.fallbackH2Request)
				defer wlc.Close()

				if err != nil {
					if ce := utils.CanLogErr("fallback h2 RoundTrip failed"); ce != nil {
						ce.Write(zap.Error(err), zap.String("url", urlStr))
					}

					return
				}

				netLayer.TryCopy(wlc, rsp.Body)

				return
			}

			iics.fallbackXver = fbResult
		}
	}

	if wlc == nil && udp_wlc == nil {
		//无wlc证明 inServer 握手失败，且 没有任何回落可用, 直接退出。
		utils.Debug("invalid request and no matched fallback, hung up")
		if iics.wrappedConn != nil {
			iics.wrappedConn.Close()

		}
		return
	}

	//此时 targetAddr已经完全确定

	////////////////////////////// DNS解析阶段 /////////////////////////////////////

	//dns解析会试图解析域名并将ip放入 targetAddr中
	// 因为在direct时，netLayer.Addr 拨号时，会优先选用ip拨号，而且我们下面的分流阶段 如果使用ip的话，
	// 可以利用geoip文件,  可以做到国别分流.

	if iics.RoutingEnv != nil && iics.RoutingEnv.DnsMachine != nil && (targetAddr.Name != "" && len(targetAddr.IP) == 0) && targetAddr.Network != "unix" {

		if ce := utils.CanLogDebug("Dns querying"); ce != nil {
			ce.Write(zap.String("domain", targetAddr.Name))
		}

		ip := iics.RoutingEnv.DnsMachine.Query(targetAddr.Name)

		if ip != nil {
			targetAddr.IP = ip

			if ce2 := utils.CanLogDebug("Dns result"); ce2 != nil {
				ce2.Write(zap.String("domain", targetAddr.Name), zap.String("ip", ip.String()))
			}
		}
	}

	////////////////////////////// 分流阶段 /////////////////////////////////////

	var client proxy.Client = iics.defaultClient
	routed := false

	inServer := iics.inServer

	//尝试分流, 获取到真正要发向 的 outClient
	if iics.RoutingEnv != nil && iics.RoutingEnv.RoutePolicy != nil && !(inServer != nil && inServer.CantRoute()) {

		desc := &netLayer.TargetDescription{
			Addr: targetAddr,
		}
		if inServer != nil {
			desc.Tag = inServer.GetTag()
		}

		if ce := utils.CanLogDebug("try routing"); ce != nil {
			ce.Write(zap.Any("source", desc))
		}

		outtag := iics.RoutingEnv.RoutePolicy.GetOutTag(desc)

		if outtag == proxy.DirectName {
			client = DirectClient
			iics.routedToDirect = true
			routed = true

			if ce := utils.CanLogInfo("Route to direct"); ce != nil {
				ce.Write(
					zap.String("target", targetAddr.UrlString()),
				)
			}
		} else {

			if len(iics.RoutingEnv.ClientsTagMap) > 0 {
				if tagC, ok := iics.RoutingEnv.ClientsTagMap[outtag]; ok {
					client = tagC
					routed = true
					if ce := utils.CanLogInfo("Route"); ce != nil {
						ce.Write(
							zap.String("to outtag", outtag),
							zap.String("with addr", client.AddrStr()),
							zap.String("and protocol", proxy.GetFullName(client)),
							zap.Any("for source", desc),
						)
					}
				}
			}

		}
	}

	if !routed {
		if ce := utils.CanLogDebug("Default Route"); ce != nil {
			ce.Write(
				zap.Any("source", targetAddr.String()),
				zap.String("client", proxy.GetFullName(client)),
				zap.String("addr", client.AddrStr()),
			)
		}
	}

	////////////////////////////// 特殊处理阶段 /////////////////////////////////////

	// 下面几段用于处理 tls lazy

	var isTlsLazy_clientEnd bool

	if targetAddr.IsUDP() {
		//udp数据是无法splice的，因为不是入口处是真udp就是出口处是真udp; 同样暂不考虑级连情况.
		if iics.isTlsLazyServerEnd {
			iics.isTlsLazyServerEnd = false
			//此时 inServer的tls还被包了一个Recorder，所以我们赶紧关闭记录省着产生额外开销

			iics.inServerTlsRawReadRecorder.StopRecord()
		}
	} else {
		isTlsLazy_clientEnd = Tls_lazy_encrypt && canLazyEncryptClient(client)

	}

	// 我们在客户端 lazy_encrypt 探测时，读取socks5 传来的信息，因为这个 就是要发送到 outClient 的信息，所以就不需要等包上vless、tls后再判断了, 直接解包 socks5 对 tls 进行判断
	//
	//  而在服务端探测时，因为 客户端传来的连接 包了 tls，所以要在tls解包后, vless 解包后，再进行判断；
	// 所以总之都是要在 inServer 判断 wlc; 总之，含义就是，去检索“用户承载数据”的来源

	if isTlsLazy_clientEnd || iics.isTlsLazyServerEnd {

		if tlsLayer.PDD {
			utils.Debug(fmt.Sprintf("loading TLS SniffConn %t %t\n", isTlsLazy_clientEnd, iics.isTlsLazyServerEnd))

		}

		wlc = tlsLayer.NewSniffConn(iics.baseLocalConn, wlc, isTlsLazy_clientEnd, Tls_lazy_secure)

	}

	//这一段代码是去判断是否要在转发结束后自动关闭连接, 主要是socks5 和lazy 的特殊情况

	if targetAddr.IsUDP() {

		if inServer != nil {
			switch inServer.Name() {
			case "socks5":
				// UDP Associate：
				// 因为socks5的 UDP Associate 办法是较为特殊的，不使用现有tcp而是新建立udp，所以此时该tcp连接已经没用了
				// 但是根据socks5标准，这个tcp链接同样是 keep alive的，否则客户端就会认为服务端挂掉了.
				// 另外，此时 targetAddr.IsUDP 只是用于告知此链接是udp Associate，并不包含实际地址信息
			default:
				iics.shouldCloseInSerBaseConnWhenFinish = true

			}
		}

	} else {

		//lazy_encrypt情况比较特殊，基础连接何时被关闭会在tlslazy相关代码中处理。
		// 如果不是lazy的情况的话，转发结束后，要自动关闭
		if !iics.isTlsLazyServerEnd {

			//实测 grpc.Conn 被调用了Close 也不会实际关闭连接，而是会卡住，阻塞，直到底层tcp连接被关闭后才会返回
			// 但是我们还是 直接避免这种情况
			if inServer != nil && !(inServer.GetAdvServer() != nil && inServer.GetAdvServer().IsMux()) {
				iics.shouldCloseInSerBaseConnWhenFinish = true

			}

		}

	}

	////////////////////////////// 拨号阶段 /////////////////////////////////////

	dialClient_andRelay(iics, targetAddr, client, isTlsLazy_clientEnd, wlc, udp_wlc)
}

//dialClient 对实际client进行拨号，处理传输层, tls层, 高级层等所有层级后，进行代理层握手。
// result = 0 表示拨号成功, result = -1 表示 拨号失败, result = 1 表示 拨号成功 并 已经自行处理了转发阶段(用于lazy和 innerMux ); -10 标识 因为 client为reject 而关闭了连接。
// 在 dialClient_andRelay 中被调用。在udp为multi channel时也有用到.
func dialClient(iics incomingInserverConnState, targetAddr netLayer.Addr,
	client proxy.Client,
	wlc net.Conn,
	isTlsLazy_clientEnd bool) (

	//return values:
	wrc io.ReadWriteCloser,
	udp_wrc netLayer.MsgConn,
	realTargetAddr netLayer.Addr,
	clientEndRemoteClientTlsRawReadRecorder *tlsLayer.Recorder,
	result int) {

	if client.Name() == "reject" && wlc != nil {
		client.Handshake(wlc, nil, netLayer.Addr{})
		result = -10
		return
	}

	isudp := targetAddr.IsUDP()

	hasInnerMux := false
	var innerProxyName string

	{
		var muxInt int

		if muxInt, innerProxyName = client.HasInnerMux(); muxInt == 2 {
			hasInnerMux = true

			//先过滤掉 innermux 通道已经建立的情况, 此时我们不必再次外部拨号，而是直接进行内层拨号.
			if client.InnerMuxEstablished() {
				wrc1, realudp_wrc, result1 := dialInnerProxy(client, wlc, nil, innerProxyName, targetAddr, isudp)

				if result1 == 0 {
					if wrc1 != nil {
						wrc = wrc1
					}
					if realudp_wrc != nil {
						udp_wrc = realudp_wrc
					}
					result = result1
					return
				} else {
					utils.Debug("mux failed, will redial")
				}

			}
		}
	}

	var err error
	//先确认拨号地址

	//direct的话自己是没有目的地址的，直接使用 请求的地址
	// 而其它代理的话, realTargetAddr会被设成实际配置的代理的地址
	realTargetAddr = targetAddr

	if ce := utils.CanLogInfo("Request"); ce != nil {

		ce.Write(
			zap.String("from", iics.cachedRemoteAddr),
			zap.String("target", targetAddr.UrlString()),
			zap.String("through", proxy.GetVSI_url(client)),
		)
	}

	if client.AddrStr() != "" {

		realTargetAddr, err = netLayer.NewAddr(client.AddrStr())
		if err != nil {

			if ce := utils.CanLogErr("dial client convert addr err"); ce != nil {
				ce.Write(zap.Error(err))
			}
			result = -1
			return
		}
		realTargetAddr.Network = client.Network()
	}
	var clientConn net.Conn

	var dialedCommonConn any

	not_udp_direct := !(client.Name() == proxy.DirectName && isudp)

	/*
		direct 的udp 是自己拨号的,因为要考虑到fullcone。

		不是direct的udp的话，也要分情况:
		如果是单路的, 则我们在此dial, 如果是多路复用, 则不行, 因为要复用同一个连接
		Instead, 我们要试图 取出已经拨号好了的 连接 ，获取不到现有连接后 再拨号
	*/

	adv := client.AdvancedLayer()
	advClient := client.GetAdvClient()

	var muxC advLayer.MuxClient

	if not_udp_direct {

		if adv != "" && advClient.IsMux() {

			muxC = advClient.(advLayer.MuxClient)

			if advClient.IsSuper() {

				dialedCommonConn, err = muxC.GetCommonConn(nil)
				if dialedCommonConn != nil && err == nil {
					goto advLayerHandshakeStep
				} else {

					if ce := utils.CanLogErr("Super AdvLayer GetCommonConn failed"); ce != nil {
						ce.Write(
							zap.Error(err),
						)
					}

					result = -1
					return
				}
			} else {
				dialedCommonConn, err = muxC.GetCommonConn(nil)
				if dialedCommonConn != nil && err == nil {
					goto advLayerHandshakeStep
				}
			}
		}

		clientConn, err = realTargetAddr.Dial()

		if err != nil {
			if err == netLayer.ErrMachineCantConnectToIpv6 {
				//如果一开始就知道机器没有ipv6地址，那么该错误就不是error等级，而是warning等级

				if ce := utils.CanLogWarn("Machine HasNo ipv6 but got ipv6 request"); ce != nil {
					ce.Write(
						zap.String("target", realTargetAddr.String()),
					)
				}

			} else {
				//虽然拨号失败,但是不能认为我们一定有错误, 因为很可能申请的ip本身就是不可达的, 所以不是error等级而是warn等级
				if ce := utils.CanLogWarn("failed dialing"); ce != nil {
					ce.Write(
						zap.String("target", realTargetAddr.String()),
						zap.Error(err),
					)
				}
			}
			result = -1
			return
		}

	}

	if xver := iics.fallbackXver; xver > 0 && xver < 3 {
		utils.Debug("Trying to write proxy protocol head")

		netLayer.WritePROXYprotocol(xver, wlc, clientConn)
		//log.Println("write proxy protocol result", err, wlc.LocalAddr(), wlc.RemoteAddr(), clientConn.LocalAddr(), clientConn.RemoteAddr())
	}

	////////////////////////////// tls握手阶段 /////////////////////////////////////

	if client.IsUseTLS() {

		if isTlsLazy_clientEnd {

			if Tls_lazy_secure && wlc != nil {
				// 如果使用secure办法，则我们每次不能先拨号，而是要detect用户的首包后再拨号
				// 这种情况只需要客户端操作, 此时我们wrc直接传入原始的 刚拨号好的 tcp连接，即 clientConn

				// 而且为了避免黑客攻击或探测，我们要使用uuid作为特殊指令，此时需要 UserServer和 UserClient

				if uc := client.(proxy.UserClient); uc != nil {
					tryTlsLazyRawCopy(true, uc, nil, targetAddr, clientConn, wlc, nil, true, nil)

				}

				result = 1
				return

			} else {
				clientEndRemoteClientTlsRawReadRecorder = tlsLayer.NewRecorder()
				teeConn := tlsLayer.NewTeeConn(clientConn, clientEndRemoteClientTlsRawReadRecorder)

				clientConn = teeConn
			}
		}

		tlsConn, err2 := client.GetTLS_Client().Handshake(clientConn)
		if err2 != nil {
			if ce := utils.CanLogErr("failed in handshake outClient tls"); ce != nil {
				ce.Write(zap.String("target", targetAddr.String()), zap.Error(err))
			}

			result = -1
			return
		}

		clientConn = tlsConn

	}

	////////////////////////////// header 层 /////////////////////////////////////

	if header := client.HasHeader(); header != nil && !(advClient != nil && advClient.CanHandleHeaders()) {
		clientConn = &httpLayer.HeaderConn{
			Conn: clientConn,
			H:    header,
		}

	}

	////////////////////////////// 高级层握手阶段 /////////////////////////////////////

advLayerHandshakeStep:

	if adv != "" {

		switch {

		//quic
		case advClient.IsSuper():

			clientConn, err = muxC.DialSubConn(dialedCommonConn)
			if err != nil {

				if ce := utils.CanLogErr("DialSubConn failed"); ce != nil {
					ce.Write(
						zap.Error(err),
					)
				}
				result = -1
				return

			}

		//grpc
		case advClient.IsMux():

			if dialedCommonConn == nil {
				dialedCommonConn, err = muxC.GetCommonConn(clientConn)

				if dialedCommonConn == nil {
					if ce := utils.CanLogErr("GetCommonConn failed"); ce != nil {
						ce.Write(
							zap.Error(err),
						)
					}
					result = -1
					return
				}
			}

			clientConn, err = muxC.DialSubConn(dialedCommonConn)
			if err != nil {
				if ce := utils.CanLogErr("grpc.DialNewSubConn failed"); ce != nil {

					ce.Write(zap.Error(err))

					//如果底层tcp连接被关闭了的话，错误会是：
					// rpc error: code = Unavailable desc = connection error: desc = "transport: failed to write client preface: tls: use of closed connection"

				}
				if iics.baseLocalConn != nil {
					iics.baseLocalConn.Close()
				}
				result = -1
				return
			}

		//ws
		default:

			var ed []byte

			if advClient.IsEarly() && wlc != nil {
				//若配置了 MaxEarlyDataLen，则我们先读一段;
				edBuf := utils.GetPacket()
				edBuf = edBuf[:advLayer.MaxEarlyDataLen]

				wlc.SetReadDeadline(time.Now().Add(proxy.FirstPayloadTimeout))
				n, e := wlc.Read(edBuf)
				wlc.SetReadDeadline(time.Time{})

				if e != nil {
					if ce := utils.CanLogErr("failed to read Single AdvLayer early data"); ce != nil {
						ce.Write(
							zap.String("advLayer", adv),
							zap.Error(e),
						)
					}
					result = -1
					return
				}
				if n > 0 {
					//firstPayloadAlreadyDealt = true
					ed = edBuf[:n]

				}

				if ce := utils.CanLogDebug("will send early data"); ce != nil {
					ce.Write(
						zap.String("advLayer", adv),
						zap.Int("len", n),
					)
				}

			}

			// 我们verysimple的架构是 ws握手之后，再进行vless握手
			// 但是如果要传输earlydata的话，则必须要在握手阶段就 预知 vless 的所有数据才行
			// 所以我们需要一种特殊方法

			var wc net.Conn

			wcs := advClient.(advLayer.SingleClient)

			wc, err = wcs.Handshake(clientConn, ed)

			if err != nil {
				if ce := utils.CanLogErr("failed in handshake Single AdvLayer"); ce != nil {
					ce.Write(
						zap.String("advLayer", adv),
						zap.String("target", targetAddr.String()),
						zap.Error(err),
					)
				}
				result = -1
				return
			}

			clientConn = wc
		}
	}

	////////////////////////////// 代理层 握手阶段 /////////////////////////////////////

	if !isudp || hasInnerMux {
		//udp但是有innermux时 依然用handshake, 而不是 EstablishUDPChannel
		var firstPayload []byte

		//读取firstPayload
		if !hasInnerMux {
			//如果有内层mux，要在dialInnerProxy函数里再读, 而不是在这里读

			if iics.theFallbackFirstBuffer != nil {

				firstPayload = iics.theFallbackFirstBuffer.Bytes()

			} else {
				firstPayload = utils.GetMTU()

				wlc.SetReadDeadline(time.Now().Add(proxy.FirstPayloadTimeout))
				n, err := wlc.Read(firstPayload)
				wlc.SetReadDeadline(time.Time{})

				if err != nil {

					if !errors.Is(err, os.ErrDeadlineExceeded) {
						if ce := utils.CanLogErr("Read first payload failed not because of timeout, will hung up"); ce != nil {
							ce.Write(
								zap.String("target", targetAddr.String()),
								zap.Error(err),
							)
						}

						clientConn.Close()
						wlc.Close()
						result = -1
						return
					} else {
						if ce := utils.CanLogWarn("Read first payload but timeout, will relay without first payload."); ce != nil {
							ce.Write(
								zap.String("target", targetAddr.String()),
								zap.Error(err),
							)
						}
					}

				}

				firstPayload = firstPayload[:n]
			}

		}

		if len(firstPayload) > 0 {
			if ce := utils.CanLogDebug("handshake client with first payload"); ce != nil {
				ce.Write(
					zap.Int("len", len(firstPayload)),
				)
			}
		}

		wrc, err = client.Handshake(clientConn, firstPayload, targetAddr)
		if err != nil {
			if ce := utils.CanLogErr("Handshake client failed"); ce != nil {
				ce.Write(
					zap.String("target", targetAddr.String()),
					zap.Error(err),
				)
			}
			result = -1
			return
		}

	} else {

		udp_wrc, err = client.EstablishUDPChannel(clientConn, targetAddr)
		if err != nil {
			if ce := utils.CanLogErr("EstablishUDPChannel failed"); ce != nil {
				ce.Write(
					zap.String("target", targetAddr.String()),
					zap.Error(err),
				)
			}
			result = -1
			return
		}
	}

	////////////////////////////// 建立内层 mux 阶段 /////////////////////////////////////
	if hasInnerMux {
		//我们目前的实现中，mux统一使用smux v1, 即 smux.DefaultConfig返回的值。这可以兼容trojan的实现。

		wrc, udp_wrc, result = dialInnerProxy(client, wlc, wrc, innerProxyName, targetAddr, isudp)
	}

	return
} //dialClient

//在 dialClient 中调用。 如果调用不成功，则result < 0. 若成功, 则 result == 0.
func dialInnerProxy(client proxy.Client, wlc net.Conn, wrc io.ReadWriteCloser, innerProxyName string, targetAddr netLayer.Addr, isudp bool) (realwrc io.ReadWriteCloser, realudp_wrc netLayer.MsgConn, result int) {

	smuxSession := client.GetClientInnerMuxSession(wrc)
	if smuxSession == nil {
		result = -1
		utils.Debug("dialInnerProxy return fail 1")
		return
	}

	stream, err := smuxSession.OpenStream()
	if err != nil {
		client.CloseInnerMuxSession() //发现就算 OpenStream 失败, session也不会自动被关闭, 需要我们手动关一下。

		if ce := utils.CanLogDebug("dialInnerProxy return fail 2"); ce != nil {
			ce.Write(zap.Error(err))
		}
		result = -1
		return
	}

	muxDialConf := proxy.DialConf{
		CommonConf: proxy.CommonConf{
			Protocol: innerProxyName,
		},
	}

	muxClient, err := proxy.NewClient(&muxDialConf)
	if err != nil {
		if ce := utils.CanLogDebug("mux inner proxy client creation failed"); ce != nil {
			ce.Write(zap.Error(err))
		}
		result = -1
		return
	}
	if isudp {
		realudp_wrc, err = muxClient.EstablishUDPChannel(stream, targetAddr)
		if err != nil {
			if ce := utils.CanLogDebug("mux inner proxy client handshake failed"); ce != nil {
				ce.Write(zap.Error(err))
			}
			result = -1
			return
		}
	} else {

		firstPayload := utils.GetMTU()

		wlc.SetReadDeadline(time.Now().Add(proxy.FirstPayloadTimeout))
		n, err := wlc.Read(firstPayload)

		if err != nil {
			if ce := utils.CanLogErr("Read innermux first payload failed"); ce != nil {
				ce.Write(
					zap.String("target", targetAddr.String()),
					zap.Error(err),
				)
			}

			if !errors.Is(err, os.ErrDeadlineExceeded) {
				if ce := utils.CanLogErr("Read innermux first payload failed not because of timeout, will hung up"); ce != nil {
					ce.Write(
						zap.String("target", targetAddr.String()),
						zap.Error(err),
					)
				}

				stream.Close()
				wlc.Close()
				result = -1
				return
			}

		} else {
			if ce := utils.CanLogDebug("innerMux got first payload"); ce != nil {
				ce.Write(
					zap.String("target", targetAddr.String()),
					zap.Int("payloadLen", n),
				)
			}
		}
		wlc.SetReadDeadline(time.Time{})

		realwrc, err = muxClient.Handshake(stream, firstPayload[:n], targetAddr)
		if err != nil {
			if ce := utils.CanLogDebug("mux inner proxy client handshake failed"); ce != nil {
				ce.Write(zap.Error(err))
			}
			result = -1
			return
		}
	}

	return
} //dialInnerProxy

// dialClient_andRelay 进行实际转发(Copy)。被 passToOutClient 调用.
// targetAddr为用户所请求的地址。
// client为真实要拨号的client，可能会与iics里的defaultClient不同。以client为准。
// wlc为调用者所提供的 此请求的 来源 链接
func dialClient_andRelay(iics incomingInserverConnState, targetAddr netLayer.Addr, client proxy.Client, isTlsLazy_clientEnd bool, wlc net.Conn, udp_wlc netLayer.MsgConn) {

	//在内层mux时, 不能因为单个传输完毕就关闭整个连接
	if innerMuxResult, _ := client.HasInnerMux(); innerMuxResult == 0 {
		if iics.shouldCloseInSerBaseConnWhenFinish {
			if iics.baseLocalConn != nil {
				defer iics.baseLocalConn.Close()
			}
		}

		if wlc != nil {
			defer wlc.Close()

		}
	}

	wrc, udp_wrc, realTargetAddr, clientEndRemoteClientTlsRawReadRecorder, result := dialClient(iics, targetAddr, client, wlc, isTlsLazy_clientEnd)
	if result != 0 {
		return
	}

	////////////////////////////// 实际转发阶段 /////////////////////////////////////

	if !targetAddr.IsUDP() {

		if Tls_lazy_encrypt && !iics.routedToDirect {

			// 我们加了回落之后，就无法确定 “未使用tls的outClient 一定是在服务端” 了
			if isTlsLazy_clientEnd {

				if client.IsUseTLS() {
					//必须是 UserClient
					if userClient := client.(proxy.UserClient); userClient != nil {
						tryTlsLazyRawCopy(false, userClient, nil, netLayer.Addr{}, wrc, wlc, iics.baseLocalConn, true, clientEndRemoteClientTlsRawReadRecorder)
						return
					}
				}

			} else if iics.isTlsLazyServerEnd {

				// 最新代码已经确认，使用uuid 作为 “特殊指令”，所以要求Server必须是一个 proxy.UserServer
				// 否则将无法开启splice功能。这是为了防止0-rtt 探测;

				if userServer, ok := iics.inServer.(proxy.UserServer); ok {
					tryTlsLazyRawCopy(false, nil, userServer, netLayer.Addr{}, wrc, wlc, iics.baseLocalConn, false, iics.inServerTlsRawReadRecorder)
					return
				}

			}

		}

		atomic.AddInt32(&ActiveConnectionCount, 1)

		netLayer.Relay(&realTargetAddr, wrc, wlc, &AllDownloadBytesSinceStart, &AllUploadBytesSinceStart)

		atomic.AddInt32(&ActiveConnectionCount, -1)

		return

	} else {

		if ffb := iics.theFallbackFirstBuffer; ffb != nil {
			udp_wrc.WriteMsgTo(ffb.Bytes(), targetAddr)
		}

		atomic.AddInt32(&ActiveConnectionCount, 1)

		if client.IsUDP_MultiChannel() {
			utils.Debug("Relaying UDP with MultiChannel")

			netLayer.RelayUDP_separate(udp_wrc, udp_wlc, &targetAddr, &AllDownloadBytesSinceStart, &AllUploadBytesSinceStart, func(raddr netLayer.Addr) netLayer.MsgConn {
				utils.Debug("Relaying UDP with MultiChannel,dialfunc called")

				_, udp_wrc, _, _, result := dialClient(iics, raddr, client, nil, false)

				if ce := utils.CanLogDebug("Relaying UDP with MultiChannel, dialfunc call returned"); ce != nil {
					ce.Write(zap.Int("result", result))
				}

				if result == 0 {
					return udp_wrc

				}
				return nil
			})

		} else {
			netLayer.RelayUDP(udp_wrc, udp_wlc, &AllDownloadBytesSinceStart, &AllUploadBytesSinceStart)

		}

		atomic.AddInt32(&ActiveConnectionCount, -1)

		return
	}

}
