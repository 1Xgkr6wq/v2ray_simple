package trojan

import (
	"bytes"
	"io"
	"net"
	"net/url"
	"time"

	"github.com/e1732a364fed/v2ray_simple/netLayer"
	"github.com/e1732a364fed/v2ray_simple/proxy"
	"github.com/e1732a364fed/v2ray_simple/utils"
)

func init() {
	proxy.RegisterServer(Name, &ServerCreator{})
}

type ServerCreator struct{}

func (ServerCreator) NewServer(lc *proxy.ListenConf) (proxy.Server, error) {
	uuidStr := lc.Uuid

	s := &Server{
		userHashes: make(map[string]bool),
	}

	s.userHashes[SHA224_hexString(uuidStr)] = true

	return s, nil
}

func (ServerCreator) NewServerFromURL(url *url.URL) (proxy.Server, error) {
	uuidStr := url.User.Username()
	s := &Server{
		userHashes: make(map[string]bool),
	}

	s.userHashes[SHA224_hexString(uuidStr)] = true

	return s, nil
}

//implements proxy.Server
type Server struct {
	proxy.ProxyCommonStruct

	userHashes map[string]bool

	//mux4Hashes sync.RWMutex
}

func (*Server) Name() string {
	return Name
}

func (*Server) HasInnerMux() (int, string) {
	return 1, "simplesocks"
}

//若握手步骤数据不对, 会返回 ErrDetail 为 utils.ErrInvalidData 的 utils.ErrInErr
func (s *Server) Handshake(underlay net.Conn) (result net.Conn, msgConn netLayer.MsgConn, targetAddr netLayer.Addr, returnErr error) {
	if err := underlay.SetReadDeadline(time.Now().Add(time.Second * 4)); err != nil {
		returnErr = err
		return
	}
	defer underlay.SetReadDeadline(time.Time{})

	readbs := utils.GetBytes(utils.MTU)

	wholeReadLen, err := underlay.Read(readbs)
	if err != nil {
		returnErr = utils.ErrInErr{ErrDesc: "read underlay failed", ErrDetail: err, Data: wholeReadLen}
		return
	}

	if wholeReadLen < 17 {
		//根据下面回答，HTTP的最小长度恰好是16字节，但是是0.9版本。1.0是18字节，1.1还要更长。总之我们可以直接不返回fallback地址
		//https://stackoverflow.com/questions/25047905/http-request-minimum-size-in-bytes/25065089

		returnErr = utils.ErrInErr{ErrDesc: "fallback, msg too short", Data: wholeReadLen}
		return
	}

	readbuf := bytes.NewBuffer(readbs[:wholeReadLen])

	goto realPart

errorPart:

	//所返回的 buffer 必须包含所有数据，而 bytes.Buffer 是不支持回退的，所以只能重新 New
	returnErr = &utils.ErrBuffer{
		Err: returnErr,
		Buf: bytes.NewBuffer(readbs[:wholeReadLen]),
	}
	return

realPart:

	if wholeReadLen < 56+8+1 {
		returnErr = utils.ErrInErr{ErrDesc: "handshake len too short", ErrDetail: utils.ErrInvalidData, Data: wholeReadLen}
		goto errorPart
	}

	//可参考 https://github.com/p4gefau1t/trojan-go/blob/master/tunnel/trojan/server.go

	hash := readbuf.Next(56)
	hashStr := string(hash)
	if !s.userHashes[hashStr] {
		returnErr = utils.ErrInErr{ErrDesc: "hash not match", ErrDetail: utils.ErrInvalidData, Data: hashStr}
		goto errorPart
	}

	crb, _ := readbuf.ReadByte()
	lfb, _ := readbuf.ReadByte()
	if crb != crlf[0] || lfb != crlf[1] {
		returnErr = utils.ErrInErr{ErrDesc: "crlf wrong", ErrDetail: utils.ErrInvalidData, Data: int(crb)<<8 + int(lfb)}
		goto errorPart
	}

	cmdb, _ := readbuf.ReadByte()

	var isudp, ismux bool
	switch cmdb {
	default:
		returnErr = utils.ErrInErr{ErrDesc: "cmd byte wrong", ErrDetail: utils.ErrInvalidData, Data: cmdb}
		goto errorPart
	case CmdConnect:

	case CmdUDPAssociate:
		isudp = true

	case CmdMux:
		//trojan-gfw 那个文档里并没有提及Mux, 这个定义作者似乎没有在任何文档中提及，所以是在go文件中找到的。
		// 根据 tunnel/trojan/server.go, 如果申请的域名是 MUX_CONN, 则 就算是CmdConnect 也会被认为是mux

		//关于 trojan实现多路复用的方式，可参考 https://p4gefau1t.github.io/trojan-go/developer/mux/
		ismux = true
	}

	targetAddr, err = GetAddrFrom(readbuf, ismux)
	if err != nil {
		returnErr = err
		goto errorPart
	}
	if isudp {
		targetAddr.Network = "udp"
	}
	crb, err = readbuf.ReadByte()
	if err != nil {
		returnErr = err
		goto errorPart
	}
	lfb, err = readbuf.ReadByte()
	if err != nil {
		returnErr = err
		goto errorPart
	}
	if crb != crlf[0] || lfb != crlf[1] {
		returnErr = utils.ErrInErr{ErrDesc: "crlf wrong", ErrDetail: utils.ErrInvalidData, Data: int(crb)<<8 + int(lfb)}
		goto errorPart
	}

	if ismux {
		mh := &proxy.MuxMarkerConn{
			ReadWrapper: netLayer.ReadWrapper{
				Conn: underlay,
			},
		}

		if l := readbuf.Len(); l > 0 {
			mh.RemainFirstBufLen = l
			mh.OptionalReader = io.MultiReader(readbuf, underlay)
		}

		return mh, nil, targetAddr, nil
	}

	if isudp {
		return nil, NewUDPConn(underlay, io.MultiReader(readbuf, underlay)), targetAddr, nil

	} else {
		// 发现直接返回 underlay 反倒无法利用readv, 所以还是统一用包装过的. 目前利用readv是可以加速的.

		return &UserTCPConn{
			Conn:              underlay,
			optionalReader:    io.MultiReader(readbuf, underlay),
			remainFirstBufLen: readbuf.Len(),
			hash:              hashStr,
			underlayIsBasic:   netLayer.IsBasicConn(underlay),
			isServerEnd:       true,
		}, nil, targetAddr, nil

	}
}
