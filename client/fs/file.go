// Copyright 2018 The Chubao Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package fs

import (
	"fmt"
	"io"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"

	"sync"

	"github.com/chubaofs/chubaofs/proto"
	"github.com/chubaofs/chubaofs/util/exporter"
	"github.com/chubaofs/chubaofs/util/log"
)

// File defines the structure of a file.
type File struct {
	super *Super
	info  *proto.InodeInfo
	sync.RWMutex
}

// Functions that File needs to implement
var (
	_ fs.Node              = (*File)(nil)
	_ fs.Handle            = (*File)(nil)
	_ fs.NodeForgetter     = (*File)(nil)
	_ fs.NodeOpener        = (*File)(nil)
	_ fs.HandleReleaser    = (*File)(nil)
	_ fs.HandleReader      = (*File)(nil)
	_ fs.HandleWriter      = (*File)(nil)
	_ fs.HandleFlusher     = (*File)(nil)
	_ fs.NodeFsyncer       = (*File)(nil)
	_ fs.NodeSetattrer     = (*File)(nil)
	_ fs.NodeReadlinker    = (*File)(nil)
	_ fs.NodeGetxattrer    = (*File)(nil)
	_ fs.NodeListxattrer   = (*File)(nil)
	_ fs.NodeSetxattrer    = (*File)(nil)
	_ fs.NodeRemovexattrer = (*File)(nil)
)

// NewFile returns a new file.
func NewFile(s *Super, i *proto.InodeInfo) fs.Node {
	return &File{super: s, info: i}
}

// Attr sets the attributes of a file.
func (f *File) Attr(ctx context.Context, a *fuse.Attr) error {
	ino := f.info.Inode
	info, err := f.super.InodeGet(ino)
	if err != nil {
		log.LogErrorf("Attr: ino(%v) err(%v)", ino, err)
		if err == fuse.ENOENT {
			a.Inode = ino
			return nil
		}
		return ParseError(err)
	}

	fillAttr(info, a)
	fileSize, gen := f.fileSize(ino)
	log.LogDebugf("Attr: ino(%v) fileSize(%v) gen(%v) inode.gen(%v)", ino, fileSize, gen, info.Generation)
	if gen >= info.Generation {
		a.Size = uint64(fileSize)
	}
	if proto.IsSymlink(info.Mode) {
		a.Size = uint64(len(info.Target))
	}

	log.LogDebugf("TRACE Attr: inode(%v) attr(%v)", info, a)
	return nil
}

// Forget evicts the inode of the current file. This can only happen when the inode is on the orphan list.
func (f *File) Forget() {
	ino := f.info.Inode
	defer func() {
		log.LogDebugf("TRACE Forget: ino(%v)", ino)
	}()

	f.super.ic.Delete(ino)

	f.super.fslock.Lock()
	// TODO 从缓存移除
	delete(f.super.nodeCache, ino)
	f.super.fslock.Unlock()

	if err := f.super.ec.EvictStream(ino); err != nil {
		log.LogWarnf("Forget: stream not ready to evict, ino(%v) err(%v)", ino, err)
		return
	}

	if !f.super.orphan.Evict(ino) {
		return
	}

	if err := f.super.mw.Evict(ino); err != nil {
		log.LogWarnf("Forget Evict: ino(%v) err(%v)", ino, err)
	}
}

// Open handles the open request.
func (f *File) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (handle fs.Handle, err error) {
	ino := f.info.Inode
	start := time.Now()

	f.super.ec.OpenStream(ino)

	f.super.ec.RefreshExtentsCache(ino)

	if f.super.keepCache {
		resp.Flags |= fuse.OpenKeepCache
	}

	elapsed := time.Since(start)
	log.LogDebugf("TRACE Open: ino(%v) req(%v) resp(%v) (%v)ns", ino, req, resp, elapsed.Nanoseconds())
	return f, nil
}

// Release handles the release request.
func (f *File) Release(ctx context.Context, req *fuse.ReleaseRequest) (err error) {
	ino := f.info.Inode
	log.LogDebugf("TRACE Release enter: ino(%v) req(%v)", ino, req)

	start := time.Now()

	//log.LogDebugf("TRACE Release close stream: ino(%v) req(%v)", ino, req)

	err = f.super.ec.CloseStream(ino)
	if err != nil {
		log.LogErrorf("Release: close writer failed, ino(%v) req(%v) err(%v)", ino, req, err)
		return fuse.EIO
	}

	f.super.ic.Delete(ino)
	elapsed := time.Since(start)
	log.LogDebugf("TRACE Release: ino(%v) req(%v) (%v)ns", ino, req, elapsed.Nanoseconds())
	return nil
}

// Read handles the read request.
func (f *File) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) (err error) {
	log.LogDebugf("TRACE Read enter: ino(%v) offset(%v) reqsize(%v) req(%v)", f.info.Inode, req.Offset, req.Size, req)

	start := time.Now()

	metric := exporter.NewTPCnt("fileread")
	defer func() {
		metric.SetWithLabels(err, map[string]string{exporter.Vol: f.super.volname})
	}()

	size, err := f.super.ec.Read(f.info.Inode, resp.Data[fuse.OutHeaderSize:], int(req.Offset), req.Size)
	if err != nil && err != io.EOF {
		msg := fmt.Sprintf("Read: ino(%v) req(%v) err(%v) size(%v)", f.info.Inode, req, err, size)
		f.super.handleError("Read", msg)
		return fuse.EIO
	}

	if size > req.Size {
		msg := fmt.Sprintf("Read: read size larger than request size, ino(%v) req(%v) size(%v)", f.info.Inode, req, size)
		f.super.handleError("Read", msg)
		return fuse.ERANGE
	}

	if size > 0 {
		resp.Data = resp.Data[:size+fuse.OutHeaderSize]
	} else if size <= 0 {
		resp.Data = resp.Data[:fuse.OutHeaderSize]
		log.LogWarnf("Read: ino(%v) offset(%v) reqsize(%v) req(%v) size(%v)", f.info.Inode, req.Offset, req.Size, req, size)
	}

	elapsed := time.Since(start)
	log.LogDebugf("TRACE Read: ino(%v) offset(%v) reqsize(%v) req(%v) size(%v) (%v)ns", f.info.Inode, req.Offset, req.Size, req, size, elapsed.Nanoseconds())
	return nil
}

// Write handles the write request.
func (f *File) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) (err error) {
	ino := f.info.Inode
	reqlen := len(req.Data)
	filesize, _ := f.fileSize(ino)

	log.LogDebugf("TRACE Write enter: ino(%v) offset(%v) len(%v) filesize(%v) flags(%v) fileflags(%v) req(%v)", ino, req.Offset, reqlen, filesize, req.Flags, req.FileFlags, req)

	if req.Offset > int64(filesize) && reqlen == 1 && req.Data[0] == 0 {
		// workaround: posix_fallocate would write 1 byte if fallocate is not supported.
		err = f.super.ec.Truncate(ino, int(req.Offset)+reqlen)
		if err == nil {
			resp.Size = reqlen
		}

		log.LogDebugf("fallocate: ino(%v) origFilesize(%v) req(%v) err(%v)", f.info.Inode, filesize, req, err)
		return
	}

	defer func() {
		f.super.ic.Delete(ino)
	}()

	var waitForFlush bool
	var flags int

	if isDirectIOEnabled(req.FileFlags) || (req.FileFlags&fuse.OpenSync != 0) {
		waitForFlush = true
		if f.super.enSyncWrite {
			flags |= proto.FlagsSyncWrite
		}
	}

	if req.FileFlags&fuse.OpenAppend != 0 {
		flags |= proto.FlagsAppend
	}

	start := time.Now()

	metric := exporter.NewTPCnt("filewrite")
	defer func() {
		metric.SetWithLabels(err, map[string]string{exporter.Vol: f.super.volname})
	}()

	size, err := f.super.ec.Write(ino, int(req.Offset), req.Data, flags)
	if err != nil {
		msg := fmt.Sprintf("Write: ino(%v) offset(%v) len(%v) err(%v)", ino, req.Offset, reqlen, err)
		f.super.handleError("Write", msg)
		return fuse.EIO
	}

	resp.Size = size
	if size != reqlen {
		log.LogErrorf("Write: ino(%v) offset(%v) len(%v) size(%v)", ino, req.Offset, reqlen, size)
	}

	if waitForFlush {
		if err = f.super.ec.Flush(ino); err != nil {
			msg := fmt.Sprintf("Write: failed to wait for flush, ino(%v) offset(%v) len(%v) err(%v) req(%v)", ino, req.Offset, reqlen, err, req)
			f.super.handleError("Wrtie", msg)
			return fuse.EIO
		}
	}

	elapsed := time.Since(start)
	log.LogDebugf("TRACE Write: ino(%v) offset(%v) len(%v) flags(%v) fileflags(%v) req(%v) (%v)ns ",
		ino, req.Offset, reqlen, req.Flags, req.FileFlags, req, elapsed.Nanoseconds())
	return nil
}

// Flush only when fsyncOnClose is enabled.
func (f *File) Flush(ctx context.Context, req *fuse.FlushRequest) (err error) {
	if !f.super.fsyncOnClose {
		return fuse.ENOSYS
	}
	log.LogDebugf("TRACE Flush enter: ino(%v)", f.info.Inode)
	start := time.Now()

	metric := exporter.NewTPCnt("filesync")
	defer func() {
		metric.SetWithLabels(err, map[string]string{exporter.Vol: f.super.volname})
	}()

	err = f.super.ec.Flush(f.info.Inode)
	if err != nil {
		msg := fmt.Sprintf("Flush: ino(%v) err(%v)", f.info.Inode, err)
		f.super.handleError("Flush", msg)
		return fuse.EIO
	}
	f.super.ic.Delete(f.info.Inode)
	elapsed := time.Since(start)
	log.LogDebugf("TRACE Flush: ino(%v) (%v)ns", f.info.Inode, elapsed.Nanoseconds())
	return nil
}

// Fsync hanldes the fsync request.
func (f *File) Fsync(ctx context.Context, req *fuse.FsyncRequest) (err error) {
	log.LogDebugf("TRACE Fsync enter: ino(%v)", f.info.Inode)
	start := time.Now()
	err = f.super.ec.Flush(f.info.Inode)
	if err != nil {
		msg := fmt.Sprintf("Fsync: ino(%v) err(%v)", f.info.Inode, err)
		f.super.handleError("Fsync", msg)
		return fuse.EIO
	}
	f.super.ic.Delete(f.info.Inode)
	elapsed := time.Since(start)
	log.LogDebugf("TRACE Fsync: ino(%v) (%v)ns", f.info.Inode, elapsed.Nanoseconds())
	return nil
}

// Setattr handles the setattr request.
func (f *File) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	ino := f.info.Inode
	start := time.Now()
	if req.Valid.Size() {
		if err := f.super.ec.Flush(ino); err != nil {
			log.LogErrorf("Setattr: truncate wait for flush ino(%v) size(%v) err(%v)", ino, req.Size, err)
			return ParseError(err)
		}
		if err := f.super.ec.Truncate(ino, int(req.Size)); err != nil {
			log.LogErrorf("Setattr: truncate ino(%v) size(%v) err(%v)", ino, req.Size, err)
			return ParseError(err)
		}
		f.super.ic.Delete(ino)
		f.super.ec.RefreshExtentsCache(ino)
	}

	info, err := f.super.InodeGet(ino)
	if err != nil {
		log.LogErrorf("Setattr: InodeGet failed, ino(%v) err(%v)", ino, err)
		return ParseError(err)
	}

	if req.Valid.Size() {
		if req.Size != info.Size {
			log.LogWarnf("Setattr: truncate ino(%v) reqSize(%v) inodeSize(%v)", ino, req.Size, info.Size)
		}
	}

	if valid := setattr(info, req); valid != 0 {
		err = f.super.mw.Setattr(ino, valid, info.Mode, info.Uid, info.Gid, info.AccessTime.Unix(),
			info.ModifyTime.Unix())
		if err != nil {
			f.super.ic.Delete(ino)
			return ParseError(err)
		}
	}

	fillAttr(info, &resp.Attr)

	elapsed := time.Since(start)
	log.LogDebugf("TRACE Setattr: ino(%v) req(%v) (%v)ns", ino, req, elapsed.Nanoseconds())
	return nil
}

// Readlink handles the readlink request.
func (f *File) Readlink(ctx context.Context, req *fuse.ReadlinkRequest) (string, error) {
	ino := f.info.Inode
	info, err := f.super.InodeGet(ino)
	if err != nil {
		log.LogErrorf("Readlink: ino(%v) err(%v)", ino, err)
		return "", ParseError(err)
	}
	log.LogDebugf("TRACE Readlink: ino(%v) target(%v)", ino, string(info.Target))
	return string(info.Target), nil
}

// Getxattr has not been implemented yet.
func (f *File) Getxattr(ctx context.Context, req *fuse.GetxattrRequest, resp *fuse.GetxattrResponse) error {
	if !f.super.enableXattr {
		return fuse.ENOSYS
	}
	ino := f.info.Inode
	name := req.Name
	size := req.Size
	pos := req.Position
	info, err := f.super.mw.XAttrGet_ll(ino, name)
	if err != nil {
		log.LogErrorf("GetXattr: ino(%v) name(%v) err(%v)", ino, name, err)
		return ParseError(err)
	}
	value := info.Get(name)
	if pos > 0 {
		value = value[pos:]
	}
	if size > 0 && size < uint32(len(value)) {
		value = value[:size]
	}
	resp.Xattr = value
	log.LogDebugf("TRACE GetXattr: ino(%v) name(%v)", ino, name)
	return nil
}

// Listxattr has not been implemented yet.
func (f *File) Listxattr(ctx context.Context, req *fuse.ListxattrRequest, resp *fuse.ListxattrResponse) error {
	if !f.super.enableXattr {
		return fuse.ENOSYS
	}
	ino := f.info.Inode
	_ = req.Size     // ignore currently
	_ = req.Position // ignore currently

	keys, err := f.super.mw.XAttrsList_ll(ino)
	if err != nil {
		log.LogErrorf("ListXattr: ino(%v) err(%v)", ino, err)
		return ParseError(err)
	}
	for _, key := range keys {
		resp.Append(key)
	}
	log.LogDebugf("TRACE Listxattr: ino(%v)", ino)
	return nil
}

// Setxattr has not been implemented yet.
func (f *File) Setxattr(ctx context.Context, req *fuse.SetxattrRequest) error {
	if !f.super.enableXattr {
		return fuse.ENOSYS
	}
	ino := f.info.Inode
	name := req.Name
	value := req.Xattr
	// TODO： implement flag to improve compatible (Mofei Zhang)
	if err := f.super.mw.XAttrSet_ll(ino, []byte(name), []byte(value)); err != nil {
		log.LogErrorf("Setxattr: ino(%v) name(%v) err(%v)", ino, name, err)
		return ParseError(err)
	}
	log.LogDebugf("TRACE Setxattr: ino(%v) name(%v)", ino, name)
	return nil
}

// Removexattr has not been implemented yet.
func (f *File) Removexattr(ctx context.Context, req *fuse.RemovexattrRequest) error {
	if !f.super.enableXattr {
		return fuse.ENOSYS
	}
	ino := f.info.Inode
	name := req.Name
	if err := f.super.mw.XAttrDel_ll(ino, name); err != nil {
		log.LogErrorf("Removexattr: ino(%v) name(%v) err(%v)", ino, name, err)
		return ParseError(err)
	}
	log.LogDebugf("TRACE RemoveXattr: ino(%v) name(%v)", ino, name)
	return nil
}

func (f *File) fileSize(ino uint64) (size int, gen uint64) {
	size, gen, valid := f.super.ec.FileSize(ino)
	log.LogDebugf("fileSize: ino(%v) fileSize(%v) gen(%v) valid(%v)", ino, size, gen, valid)

	if !valid {
		if info, err := f.super.InodeGet(ino); err == nil {
			size = int(info.Size)
			gen = info.Generation
		}
	}
	return
}
