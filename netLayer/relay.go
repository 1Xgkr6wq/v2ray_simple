package netLayer

import (
	"io"
	"net"
	"reflect"
	"sync/atomic"
	"syscall"

	"github.com/e1732a364fed/v2ray_simple/utils"
	"go.uber.org/zap"
)

// TryCopy 尝试 循环 从 readConn 读取数据并写入 writeConn, 直到错误发生。
//会接连尝试 splice、循环readv 以及 原始Copy方法。如果 UseReadv 的值为false，则不会使用readv。
func TryCopy(writeConn io.Writer, readConn io.Reader) (allnum int64, err error) {
	var multiWriter utils.MultiWriter

	var rawReadConn syscall.RawConn
	var isWriteConn_MultiWriter bool

	var mr utils.MultiReader

	var readv_withMultiReader bool

	if ce := utils.CanLogDebug("TryCopy"); ce != nil {
		ce.Write(
			zap.String("from", reflect.TypeOf(readConn).String()),
			zap.String("->", reflect.TypeOf(writeConn).String()),
		)
	}

	if SystemCanSplice {

		rCanSplice := CanSpliceDirectly(readConn)

		if rCanSplice {
			var wCanSplice bool
			wCanSpliceDirectly := CanSpliceDirectly(writeConn)
			if wCanSpliceDirectly {
				wCanSplice = true
			} else {
				if CanSpliceEventually(writeConn) {
					wCanSplice = true
				}
			}

			if rCanSplice && wCanSplice {
				if ce := utils.CanLogDebug("copying with splice"); ce != nil {
					ce.Write()
				}

				goto copy
			}
		}

	}
	// 不全 支持splice的话，我们就考虑 read端 可 readv 的情况
	// 连readv都不让 那就直接 经典拷贝
	if !UseReadv {
		goto classic
	}

	rawReadConn = GetRawConn(readConn)

	if rawReadConn == nil {
		var ok bool
		mr, ok = readConn.(utils.MultiReader)
		if ok && mr.WillReadBuffersBenifit() {
			readv_withMultiReader = true
		} else {
			goto classic

		}
	}

	if ce := utils.CanLogDebug("copying with readv"); ce != nil {
		if readv_withMultiReader {
			ce.Write(zap.String("with", "MultiReader"))

		} else {
			ce.Write()

		}
	}

	if !IsBasicConn(writeConn) {

		multiWriter, isWriteConn_MultiWriter = writeConn.(utils.MultiWriter)

		if readv_withMultiReader && !isWriteConn_MultiWriter {
			goto classic
		}
	}

	{
		var readv_mem *readvMem

		if !readv_withMultiReader {
			readv_mem = get_readvMem()
			defer put_readvMem(readv_mem)
		}

		//这个for循环 只会通过 return 跳出, 不会落到外部
		for {
			var buffers net.Buffers

			if readv_withMultiReader {
				buffers, err = mr.ReadBuffers()

			} else {
				buffers, err = readvFrom(rawReadConn, readv_mem)

			}

			if err != nil {
				return
			}
			var thisWriteNum int64
			var writeErr error

			// vless/trojan.UserTCPConn, ws.Conn 和 grpc.multiConn 实现了 utils.MultiWriter
			// vless/trojan的 UserTCPConn 会 间接调用 ws.Conn 和 grpc.multiConn 的 WriteBuffers

			if isWriteConn_MultiWriter {
				thisWriteNum, writeErr = multiWriter.WriteBuffers(buffers)

			} else {
				// 这里不能直接使用 buffers.WriteTo, 因为它会修改buffer本身
				// 而我们为了缓存,是不能允许篡改的
				// 所以我们在确保 writeConn 不是 基本连接后, 要 自行write

				//在basic时之所以可以 WriteTo，是因为它并不会用循环读取方式, 而是用底层的writev，
				// 而writev时是不会篡改 buffers的

				//然而经实测,writev会篡改我们的buffers，会导致问题. 而且writev也毫无性能优势,
				//所以这里统一使用我们自己的函数

				thisWriteNum, writeErr = utils.BuffersWriteTo(buffers, writeConn)

			}

			allnum += thisWriteNum
			if writeErr != nil {
				err = writeErr
				return
			}

			if !readv_withMultiReader {
				buffers = utils.RecoverBuffers(buffers, readv_buffer_allocLen, ReadvSingleBufLen)

			}

		}
	}
classic:
	if ce := utils.CanLogDebug("copying with classic method"); ce != nil {
		ce.Write()
	}
copy:

	//Copy内部实现 会调用 ReadFrom, 而ReadFrom 会自动进行splice,
	// 若无splice实现则直接使用原始方法 “循环读取 并 写入”
	// 我们的 vless/trojan 和 ws 的Conn均实现了ReadFrom方法，可以最终splice
	return io.Copy(writeConn, readConn)
}

// 类似TryCopy，但是只会读写一次; 因为只读写一次，所以没办法splice
func TryCopyOnce(writeConn io.Writer, readConn io.Reader) (allnum int64, err error) {
	var buffers net.Buffers
	var rawConn syscall.RawConn

	var rm *readvMem

	if ce := utils.CanLogDebug("TryCopy"); ce != nil {
		ce.Write(
			zap.String("from", reflect.TypeOf(readConn).String()),
			zap.String("->", reflect.TypeOf(writeConn).String()),
		)
	}

	// 不全 支持splice的话，我们就考虑 read端 可 readv 的情况
	// 连readv都不让 那就直接 经典拷贝
	if !UseReadv {
		goto classic
	}

	rawConn = GetRawConn(readConn)

	if rawConn == nil {
		goto classic
	}

	if ce := utils.CanLogDebug("copying with readv"); ce != nil {
		ce.Write()
	}

	rm = get_readvMem()
	defer put_readvMem(rm)

	buffers, err = readvFrom(rawConn, rm)
	if err != nil {
		return 0, err
	}
	allnum, err = utils.BuffersWriteTo(buffers, writeConn) //buffers.WriteTo(writeConn)

	return

classic:
	if ce := utils.CanLogDebug("copying with classic method"); ce != nil {
		ce.Write()
	}

	bs := utils.GetPacket()
	n, e := readConn.Read(bs)
	if e != nil {
		utils.PutPacket(bs)
		return 0, e
	}
	n, e = writeConn.Write(bs[:n])
	utils.PutPacket(bs)
	return int64(n), e
}

// 从 rc 读取 写入到 lc ，并同时从 lc 读取写入 rc.
// 阻塞. rc是指 remoteConn, lc 是指localConn; 一般lc由自己监听的Accept产生, rc 由自己拨号产生.
// UseReadv==true 时 内部使用 TryCopy 进行拷贝,
// 会自动优选 splice，readv，不行则使用经典拷贝.
//
//拷贝完成后会主动关闭双方连接.
// 返回从 rc读取到的总字节长度（即下载流量）. 如果 downloadByteCount, uploadByteCount 给出,
// 则会 分别原子更新 上传和下载的总字节数
func Relay(realTargetAddr *Addr, rc, lc io.ReadWriteCloser, downloadByteCount, uploadByteCount *uint64) int64 {

	if utils.LogLevel == utils.Log_debug {

		rtaddrStr := realTargetAddr.String()
		go func() {
			n, e := TryCopy(rc, lc)

			utils.CanLogDebug("Relay End").Write(zap.String("direction", "L->R"),
				zap.String("target", rtaddrStr),
				zap.Int64("bytes", n),
				zap.Error(e),
			)

			lc.Close()
			rc.Close()

			if uploadByteCount != nil {
				atomic.AddUint64(uploadByteCount, uint64(n))
			}

		}()

		n, e := TryCopy(lc, rc)

		utils.CanLogDebug("Relay End").Write(zap.String("direction", "R->L"),
			zap.String("target", rtaddrStr),
			zap.Int64("bytes", n),
			zap.Error(e),
		)

		lc.Close()
		rc.Close()

		if downloadByteCount != nil {
			atomic.AddUint64(downloadByteCount, uint64(n))
		}
		return n
	} else {
		go func() {
			n, _ := TryCopy(rc, lc)

			lc.Close()
			rc.Close()

			if uploadByteCount != nil {
				atomic.AddUint64(uploadByteCount, uint64(n))
			}

		}()

		n, _ := TryCopy(lc, rc)

		lc.Close()
		rc.Close()

		if downloadByteCount != nil {
			atomic.AddUint64(downloadByteCount, uint64(n))
		}
		return n
	}

}
