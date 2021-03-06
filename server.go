package sftp

// sftp server counterpart

import (
	"encoding"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/pkg/errors"
	"strings"
)

const (
	SftpServerWorkerCount = 1
)

// Server is an SSH File Transfer Protocol (sftp) server.
// This is intended to provide the sftp subsystem to an ssh server daemon.
// This implementation currently supports most of sftp server protocol version 3,
// as specified at http://tools.ietf.org/html/draft-ietf-secsh-filexfer-02
type Server struct {
	*serverConn
	debugStream         io.Writer
	readOnly            bool
	uploadOnly          bool
	timeout             time.Duration
	pktMgr              *packetManager
	OpenFiles           map[string]*os.File
	openFilesLock       sync.RWMutex
	handleCount         int
	maxTxPacket         uint32
	serverRoot          string
	closeHandleCallback func(file *os.File) error
	openHandleCallback  func(file *os.File, osFlags int) error
}

func (svr *Server) nextHandle(f *os.File) string {
	svr.openFilesLock.Lock()
	defer svr.openFilesLock.Unlock()
	svr.handleCount++
	handle := strconv.Itoa(svr.handleCount)
	svr.OpenFiles[handle] = f
	return handle
}

func (svr *Server) closeHandle(handle string) error {
	svr.openFilesLock.Lock()
	defer svr.openFilesLock.Unlock()
	if f, ok := svr.OpenFiles[handle]; ok {
		delete(svr.OpenFiles, handle)
		return f.Close()
	}

	return syscall.EBADF
}

func (svr *Server) getHandle(handle string) (*os.File, bool) {
	svr.openFilesLock.RLock()
	defer svr.openFilesLock.RUnlock()
	f, ok := svr.OpenFiles[handle]
	return f, ok
}

func (svr *Server) path(p string) string {
	return filepath.Join(svr.serverRoot, p)
}

// modifyWorkingPaths modifies the path(s) of request packets to
// take into account the serverRoot directory
func (svr *Server) modifyWorkingPaths(pkt requestPacket) requestPacket {
	switch pkt := pkt.(type) {
	case *sshFxpRenamePacket:
		pkt.Newpath = svr.path(pkt.Newpath)
		pkt.Oldpath = svr.path(pkt.Oldpath)
	case *sshFxpSymlinkPacket:
		pkt.Linkpath = svr.path(pkt.Linkpath)
		pkt.Targetpath = svr.path(pkt.Targetpath)
	case setPath:
		pkt.setPath(svr.path(pkt.getPath()))
	}
	return pkt
}

type serverRespondablePacket interface {
	encoding.BinaryUnmarshaler
	id() uint32
	respond(svr *Server) error
}

// NewServer creates a new Server instance around the provided streams, serving
// content from the root of the filesystem.  Optionally, ServerOption
// functions may be specified to further configure the Server.
//
// A subsequent call to Serve() is required to begin serving files over SFTP.
func NewServer(rwc io.ReadWriteCloser, options ...ServerOption) (*Server, error) {
	svrConn := &serverConn{
		conn: conn{
			Reader:      rwc,
			WriteCloser: rwc,
		},
	}
	s := &Server{
		serverConn:  svrConn,
		debugStream: ioutil.Discard,
		timeout:     time.Hour,
		pktMgr:      newPktMgr(svrConn),
		OpenFiles:   make(map[string]*os.File),
		openHandleCallback: func(file *os.File, osFlags int) error {
			return nil
		},
		closeHandleCallback: func(file *os.File) error {
			return nil
		},
		serverRoot:  "/",
		maxTxPacket: 1 << 15,
	}

	for _, o := range options {
		if err := o(s); err != nil {
			return nil, err
		}
	}

	if s.readOnly && s.uploadOnly {
		return s, fmt.Errorf("server cannot be readonly and uploadonly at the same time")
	}

	return s, nil
}

// A ServerOption is a function which applies configuration to a Server.
type ServerOption func(*Server) error

// WithDebug enables Server debugging output to the supplied io.Writer.
func WithDebug(w io.Writer) ServerOption {
	return func(s *Server) error {
		s.debugStream = w
		return nil
	}
}

// ReadOnly configures a Server to serve files in read-only mode.
func ReadOnly() ServerOption {
	return func(s *Server) error {
		s.readOnly = true
		return nil
	}
}

// UploadOnly configures a Server to only allow
// - Opening of a file
// - Reading the stats (Certain clients check the path before writing)
// - Writing to that file
// - Setting the stats of that file
// - No other operations are supported
func UploadOnly() ServerOption {
	return func(s *Server) error {
		s.uploadOnly = true
		return nil
	}
}

// RootDirectory configures the root directory of a Server. Files will not be served outside this directory.
func RootDirectory(root string) ServerOption {
	return func(s *Server) error {
		s.serverRoot = root
		return nil
	}
}

// TimeoutDuration configures the duration after which an idle client is disconnected
func TimeoutDuration(timeout time.Duration) ServerOption {
	return func(s *Server) error {
		s.timeout = timeout
		return nil
	}
}

func CloseHandleCallback(f func(file *os.File) error) ServerOption {
	return func(s *Server) error {
		s.closeHandleCallback = f
		return nil
	}
}

func OpenHandleCallback(f func(file *os.File, osFlags int) error) ServerOption {
	return func(s *Server) error {
		s.openHandleCallback = f
		return nil
	}
}

type rxPacket struct {
	pktType  fxp
	pktBytes []byte
}

// Up to N parallel servers
func (svr *Server) sftpServerWorker(pktChan chan requestPacket) error {
	for pkt := range pktChan {
		pkt = svr.modifyWorkingPaths(pkt)
		// permission checks
		permiss := true
		if stat, err := os.Stat(svr.serverRoot); err == nil && stat.IsDir() {
			switch pkt := pkt.(type) {
			case *sshFxpRenamePacket:
				rel, e := filepath.Rel(svr.serverRoot, pkt.Oldpath)
				rel2, e2 := filepath.Rel(svr.serverRoot, pkt.Newpath)
				permiss = e == nil && e2 == nil && !strings.Contains(rel, "..") && !strings.Contains(rel2, "..")
			case *sshFxpSymlinkPacket:
				rel, e := filepath.Rel(svr.serverRoot, pkt.Targetpath)
				rel2, e2 := filepath.Rel(svr.serverRoot, pkt.Linkpath)
				permiss = e == nil && e2 == nil && !strings.Contains(rel, "..") && !strings.Contains(rel2, "..")
			case hasPath:
				rel, e := filepath.Rel(svr.serverRoot, pkt.getPath())
				permiss = e == nil && !strings.Contains(rel, "..")
			}
		}

		// readonly checks
		readonly := true
		if permiss {
			switch pkt := pkt.(type) {
			case notReadOnly:
				readonly = false
			case *sshFxpOpenPacket:
				readonly = pkt.readonly()
			case *sshFxpExtendedPacket:
				readonly = pkt.readonly()
			}
		}

		//simple upload restricted
		uploadRestricted := true
		if permiss {
			switch pkt := pkt.(type) {
			case *sshFxpOpenPacket:
				uploadRestricted = !pkt.hasPflags(ssh_FXF_READ)
			case
				*sshFxpReadPacket,
				*sshFxpReadlinkPacket,
				*sshFxpRemovePacket,
				*sshFxpRmdirPacket,
				*sshFxpRenamePacket:
				uploadRestricted = false
			case *sshFxpExtendedPacket:
				//block all extended packets for now
				uploadRestricted = false
			}
		}

		// If server is operating read-only and a write operation is requested
		// or if server is operating upload-only and a read operation is requested
		// or a restricted file is requested
		// return permission denied.
		if !permiss || (!readonly && svr.readOnly) || (!uploadRestricted && svr.uploadOnly) {
			if err := svr.sendError(pkt, syscall.EPERM); err != nil {
				return errors.Wrap(err, "failed to send read only packet response")
			}
			continue
		}

		if err := handlePacket(svr, pkt); err != nil {
			return err
		}
	}
	return nil
}

func handlePacket(s *Server, p interface{}) error {

	switch p := p.(type) {
	case *sshFxInitPacket:
		return s.sendPacket(sshFxVersionPacket{sftpProtocolVersion, nil})
	case *sshFxpStatPacket:
		// stat the requested file
		info, err := os.Stat(p.Path)
		if err != nil {
			return s.sendError(p, err)
		}
		return s.sendPacket(sshFxpStatResponse{
			ID:   p.ID,
			info: info,
		})
	case *sshFxpLstatPacket:
		// stat the requested file
		info, err := os.Lstat(p.Path)
		if err != nil {
			return s.sendError(p, err)
		}
		return s.sendPacket(sshFxpStatResponse{
			ID:   p.ID,
			info: info,
		})
	case *sshFxpFstatPacket:
		f, ok := s.getHandle(p.Handle)
		if !ok {
			return s.sendError(p, syscall.EBADF)
		}

		info, err := f.Stat()
		if err != nil {
			return s.sendError(p, err)
		}

		return s.sendPacket(sshFxpStatResponse{
			ID:   p.ID,
			info: info,
		})
	case *sshFxpMkdirPacket:
		// TODO FIXME: ignore flags field
		err := os.Mkdir(p.Path, 0755)
		return s.sendError(p, err)
	case *sshFxpRmdirPacket:
		err := os.Remove(p.Path)
		return s.sendError(p, err)
	case *sshFxpRemovePacket:
		err := os.Remove(p.Filename)
		return s.sendError(p, err)
	case *sshFxpRenamePacket:
		err := os.Rename(p.Oldpath, p.Newpath)
		return s.sendError(p, err)
	case *sshFxpSymlinkPacket:
		err := os.Symlink(p.Targetpath, p.Linkpath)
		return s.sendError(p, err)
	case *sshFxpClosePacket:
		errCallback := s.closeHandleCallback(s.OpenFiles[p.Handle])
		// allow the server to close the handle even if the callback failed.
		errClose := s.closeHandle(p.Handle)
		if errCallback == nil {
			return s.sendError(p, errClose)
		}
		return s.sendError(p, errCallback)

	case *sshFxpReadlinkPacket:
		f, err := os.Readlink(p.Path)
		if err != nil {
			return s.sendError(p, err)
		}

		return s.sendPacket(sshFxpNamePacket{
			ID: p.ID,
			NameAttrs: []sshFxpNameAttr{{
				Name:     f,
				LongName: f,
				Attrs:    emptyFileStat,
			}},
		})

	case *sshFxpRealpathPacket:
		f, err := filepath.Abs(p.Path)
		if err != nil {
			return s.sendError(p, err)
		}
		f, err = filepath.Rel(s.serverRoot, f)
		if err != nil {
			return s.sendError(p, err)
		}
		f = cleanPath(f)
		return s.sendPacket(sshFxpNamePacket{
			ID: p.ID,
			NameAttrs: []sshFxpNameAttr{{
				Name:     f,
				LongName: f,
				Attrs:    emptyFileStat,
			}},
		})
	case *sshFxpOpendirPacket:
		return sshFxpOpenPacket{
			ID:     p.ID,
			Path:   p.Path,
			Pflags: ssh_FXF_READ,
		}.respond(s)
	case *sshFxpReadPacket:
		f, ok := s.getHandle(p.Handle)
		if !ok {
			return s.sendError(p, syscall.EBADF)
		}

		data := make([]byte, clamp(p.Len, s.maxTxPacket))
		n, err := f.ReadAt(data, int64(p.Offset))
		if err != nil && (err != io.EOF || n == 0) {
			return s.sendError(p, err)
		}
		return s.sendPacket(sshFxpDataPacket{
			ID:     p.ID,
			Length: uint32(n),
			Data:   data[:n],
		})
	case *sshFxpWritePacket:
		f, ok := s.getHandle(p.Handle)
		if !ok {
			return s.sendError(p, syscall.EBADF)
		}

		_, err := f.WriteAt(p.Data, int64(p.Offset))
		return s.sendError(p, err)
	case serverRespondablePacket:
		err := p.respond(s)
		return errors.Wrap(err, "pkt.respond failed")
	default:
		return errors.Errorf("unexpected packet type %T", p)
	}
}

// Serve serves SFTP connections until the streams stop or the SFTP subsystem
// is stopped or client times out
func (svr *Server) Serve() error {
	var wg sync.WaitGroup
	runWorker := func(ch requestChan) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svr.sftpServerWorker(ch); err != nil {
				svr.conn.Close() // shuts down recvPacket
			}
		}()
	}
	pktChan := svr.pktMgr.workerChan(runWorker)

	var err error
	var pkt requestPacket
	var pktType uint8
	var pktBytes []byte

	type pktData struct {
		pktType  uint8
		pktBytes []byte
		err      error
	}

	pktDataChan := make(chan pktData, 1)
	quit := make(chan bool)

	go func(c chan pktData, quit chan bool) {
		for {
			select {
			case <-quit:
				close(pktDataChan)
				return
			default:
				pktType, pktBytes, err = svr.recvPacket()
				c <- pktData{pktType, pktBytes, err}
			}
		}
	}(pktDataChan, quit)

L:
	for {
		select {
		case data := <-pktDataChan:
			if data.err != nil {
				break L
			}
			pkt, err = makePacket(rxPacket{fxp(data.pktType), data.pktBytes})
			if err != nil {
				switch errors.Cause(err) {
				case errUnknownExtendedPacket:
					if err := svr.serverConn.sendError(pkt, ErrSshFxOpUnsupported); err != nil {
						debug("failed to send err packet: %v", err)
						svr.conn.Close() // shuts down recvPacket
						break
					}
				default:
					debug("makePacket err: %v", err)
					close(quit)
					svr.conn.Close() // shuts down recvPacket
					break
				}
			}

			pktChan <- pkt

		case <-time.After(svr.timeout):
			err = fmt.Errorf("client timed out")
			close(quit)
			svr.conn.Close() // shuts down recvPacket
			break L
		}
	}

	close(pktChan) // shuts down sftpServerWorkers
	wg.Wait()      // wait for all workers to exit

	// close any still-open files
	for handle, file := range svr.OpenFiles {
		//force call the closeHandleCallback to clean up before closing the file handle
		svr.closeHandleCallback(file)
		fmt.Fprintf(svr.debugStream, "sftp server file with handle %q left open: %v\n", handle, file.Name())
		file.Close()
	}
	return err // error from recvPacket
}

// Wrap underlying connection methods to use packetManager
func (svr *Server) sendPacket(m encoding.BinaryMarshaler) error {
	if pkt, ok := m.(responsePacket); ok {
		svr.pktMgr.readyPacket(pkt)
	} else {
		return errors.Errorf("unexpected packet type %T", m)
	}
	return nil
}

func (svr *Server) sendError(p ider, err error) error {
	return svr.sendPacket(statusFromError(p, err))
}

type ider interface {
	id() uint32
}

// The init packet has no ID, so we just return a zero-value ID
func (p sshFxInitPacket) id() uint32 { return 0 }

type sshFxpStatResponse struct {
	ID   uint32
	info os.FileInfo
}

func (p sshFxpStatResponse) MarshalBinary() ([]byte, error) {
	b := []byte{ssh_FXP_ATTRS}
	b = marshalUint32(b, p.ID)
	b = marshalFileInfo(b, p.info)
	return b, nil
}

var emptyFileStat = []interface{}{uint32(0)}

func (p sshFxpOpenPacket) readonly() bool {
	return !p.hasPflags(ssh_FXF_WRITE)
}

func (p sshFxpOpenPacket) hasPflags(flags ...uint32) bool {
	for _, f := range flags {
		if p.Pflags&f == 0 {
			return false
		}
	}
	return true
}

func (p sshFxpOpenPacket) respond(svr *Server) error {
	var osFlags int
	if p.hasPflags(ssh_FXF_READ, ssh_FXF_WRITE) {
		osFlags |= os.O_RDWR
	} else if p.hasPflags(ssh_FXF_WRITE) {
		osFlags |= os.O_WRONLY
	} else if p.hasPflags(ssh_FXF_READ) {
		osFlags |= os.O_RDONLY
	} else {
		// how are they opening?
		return svr.sendError(p, syscall.EINVAL)
	}

	if p.hasPflags(ssh_FXF_APPEND) {
		osFlags |= os.O_APPEND
	}
	if p.hasPflags(ssh_FXF_CREAT) {
		osFlags |= os.O_CREATE
	}
	if p.hasPflags(ssh_FXF_TRUNC) {
		osFlags |= os.O_TRUNC
	}
	if p.hasPflags(ssh_FXF_EXCL) {
		osFlags |= os.O_EXCL
	}
	f, err := os.OpenFile(p.Path, osFlags, 0644)
	if err != nil {
		return svr.sendError(p, err)
	}

	handle := svr.nextHandle(f)
	if err := svr.openHandleCallback(svr.OpenFiles[handle], osFlags); err != nil {
		svr.closeHandle(handle)
		return svr.sendError(p, err)
	}
	return svr.sendPacket(sshFxpHandlePacket{p.ID, handle})
}

func (p sshFxpReaddirPacket) respond(svr *Server) error {
	f, ok := svr.getHandle(p.Handle)
	if !ok {
		return svr.sendError(p, syscall.EBADF)
	}

	dirname := f.Name()
	dirents, err := f.Readdir(128)
	if err != nil {
		return svr.sendError(p, err)
	}

	ret := sshFxpNamePacket{ID: p.ID}
	for _, dirent := range dirents {
		ret.NameAttrs = append(ret.NameAttrs, sshFxpNameAttr{
			Name:     dirent.Name(),
			LongName: runLs(dirname, dirent),
			Attrs:    []interface{}{dirent},
		})
	}
	return svr.sendPacket(ret)
}

func (p sshFxpSetstatPacket) respond(svr *Server) error {
	// additional unmarshalling is required for each possibility here
	b := p.Attrs.([]byte)
	var err error

	debug("setstat name \"%s\"", p.Path)
	if (p.Flags & ssh_FILEXFER_ATTR_SIZE) != 0 {
		var size uint64
		if size, b, err = unmarshalUint64Safe(b); err == nil {
			err = os.Truncate(p.Path, int64(size))
		}
	}
	if (p.Flags & ssh_FILEXFER_ATTR_PERMISSIONS) != 0 {
		var mode uint32
		if mode, b, err = unmarshalUint32Safe(b); err == nil {
			err = os.Chmod(p.Path, os.FileMode(mode))
		}
	}
	if (p.Flags & ssh_FILEXFER_ATTR_ACMODTIME) != 0 {
		var atime uint32
		var mtime uint32
		if atime, b, err = unmarshalUint32Safe(b); err != nil {
		} else if mtime, b, err = unmarshalUint32Safe(b); err != nil {
		} else {
			atimeT := time.Unix(int64(atime), 0)
			mtimeT := time.Unix(int64(mtime), 0)
			err = os.Chtimes(p.Path, atimeT, mtimeT)
		}
	}
	if (p.Flags & ssh_FILEXFER_ATTR_UIDGID) != 0 {
		var uid uint32
		var gid uint32
		if uid, b, err = unmarshalUint32Safe(b); err != nil {
		} else if gid, _, err = unmarshalUint32Safe(b); err != nil {
		} else {
			err = os.Chown(p.Path, int(uid), int(gid))
		}
	}

	return svr.sendError(p, err)
}

func (p sshFxpFsetstatPacket) respond(svr *Server) error {
	f, ok := svr.getHandle(p.Handle)
	if !ok {
		return svr.sendError(p, syscall.EBADF)
	}

	// additional unmarshalling is required for each possibility here
	b := p.Attrs.([]byte)
	var err error

	debug("fsetstat name \"%s\"", f.Name())
	if (p.Flags & ssh_FILEXFER_ATTR_SIZE) != 0 {
		var size uint64
		if size, b, err = unmarshalUint64Safe(b); err == nil {
			err = f.Truncate(int64(size))
		}
	}
	if (p.Flags & ssh_FILEXFER_ATTR_PERMISSIONS) != 0 {
		var mode uint32
		if mode, b, err = unmarshalUint32Safe(b); err == nil {
			err = f.Chmod(os.FileMode(mode))
		}
	}
	if (p.Flags & ssh_FILEXFER_ATTR_ACMODTIME) != 0 {
		var atime uint32
		var mtime uint32
		if atime, b, err = unmarshalUint32Safe(b); err != nil {
		} else if mtime, b, err = unmarshalUint32Safe(b); err != nil {
		} else {
			atimeT := time.Unix(int64(atime), 0)
			mtimeT := time.Unix(int64(mtime), 0)
			err = os.Chtimes(f.Name(), atimeT, mtimeT)
		}
	}
	if (p.Flags & ssh_FILEXFER_ATTR_UIDGID) != 0 {
		var uid uint32
		var gid uint32
		if uid, b, err = unmarshalUint32Safe(b); err != nil {
		} else if gid, _, err = unmarshalUint32Safe(b); err != nil {
		} else {
			err = f.Chown(int(uid), int(gid))
		}
	}

	return svr.sendError(p, err)
}

// translateErrno translates a syscall error number to a SFTP error code.
func translateErrno(errno syscall.Errno) uint32 {
	switch errno {
	case 0:
		return ssh_FX_OK
	case syscall.ENOENT:
		return ssh_FX_NO_SUCH_FILE
	case syscall.EPERM:
		return ssh_FX_PERMISSION_DENIED
	}

	return ssh_FX_FAILURE
}

func statusFromError(p ider, err error) sshFxpStatusPacket {
	ret := sshFxpStatusPacket{
		ID: p.id(),
		StatusError: StatusError{
			// ssh_FX_OK                = 0
			// ssh_FX_EOF               = 1
			// ssh_FX_NO_SUCH_FILE      = 2 ENOENT
			// ssh_FX_PERMISSION_DENIED = 3
			// ssh_FX_FAILURE           = 4
			// ssh_FX_BAD_MESSAGE       = 5
			// ssh_FX_NO_CONNECTION     = 6
			// ssh_FX_CONNECTION_LOST   = 7
			// ssh_FX_OP_UNSUPPORTED    = 8
			Code: ssh_FX_OK,
		},
	}
	if err == nil {
		return ret
	}

	debug("statusFromError: error is %T %#v", err, err)
	ret.StatusError.Code = ssh_FX_FAILURE
	ret.StatusError.msg = err.Error()

	switch e := err.(type) {
	case syscall.Errno:
		ret.StatusError.Code = translateErrno(e)
	case *os.PathError:
		debug("statusFromError,pathError: error is %T %#v", e.Err, e.Err)
		if errno, ok := e.Err.(syscall.Errno); ok {
			ret.StatusError.Code = translateErrno(errno)
		}
	case fxerr:
		ret.StatusError.Code = uint32(e)
	default:
		switch e {
		case io.EOF:
			ret.StatusError.Code = ssh_FX_EOF
		case os.ErrNotExist:
			ret.StatusError.Code = ssh_FX_NO_SUCH_FILE
		}
	}

	return ret
}

func clamp(v, max uint32) uint32 {
	if v > max {
		return max
	}
	return v
}

func runLsTypeWord(dirent os.FileInfo) string {
	// find first character, the type char
	// b     Block special file.
	// c     Character special file.
	// d     Directory.
	// l     Symbolic link.
	// s     Socket link.
	// p     FIFO.
	// -     Regular file.
	tc := '-'
	mode := dirent.Mode()
	if (mode & os.ModeDir) != 0 {
		tc = 'd'
	} else if (mode & os.ModeDevice) != 0 {
		tc = 'b'
		if (mode & os.ModeCharDevice) != 0 {
			tc = 'c'
		}
	} else if (mode & os.ModeSymlink) != 0 {
		tc = 'l'
	} else if (mode & os.ModeSocket) != 0 {
		tc = 's'
	} else if (mode & os.ModeNamedPipe) != 0 {
		tc = 'p'
	}

	// owner
	orc := '-'
	if (mode & 0400) != 0 {
		orc = 'r'
	}
	owc := '-'
	if (mode & 0200) != 0 {
		owc = 'w'
	}
	oxc := '-'
	ox := (mode & 0100) != 0
	setuid := (mode & os.ModeSetuid) != 0
	if ox && setuid {
		oxc = 's'
	} else if setuid {
		oxc = 'S'
	} else if ox {
		oxc = 'x'
	}

	// group
	grc := '-'
	if (mode & 040) != 0 {
		grc = 'r'
	}
	gwc := '-'
	if (mode & 020) != 0 {
		gwc = 'w'
	}
	gxc := '-'
	gx := (mode & 010) != 0
	setgid := (mode & os.ModeSetgid) != 0
	if gx && setgid {
		gxc = 's'
	} else if setgid {
		gxc = 'S'
	} else if gx {
		gxc = 'x'
	}

	// all / others
	arc := '-'
	if (mode & 04) != 0 {
		arc = 'r'
	}
	awc := '-'
	if (mode & 02) != 0 {
		awc = 'w'
	}
	axc := '-'
	ax := (mode & 01) != 0
	sticky := (mode & os.ModeSticky) != 0
	if ax && sticky {
		axc = 't'
	} else if sticky {
		axc = 'T'
	} else if ax {
		axc = 'x'
	}

	return fmt.Sprintf("%c%c%c%c%c%c%c%c%c%c", tc, orc, owc, oxc, grc, gwc, gxc, arc, awc, axc)
}
