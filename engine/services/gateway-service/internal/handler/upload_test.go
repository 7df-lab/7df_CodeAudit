package handler

// ADR-200 回归锁：上传 = gateway 流式直传 storage（零落盘）。
// 解包/穿越防护随通道迁至 task-service（archive.go），此处锁:
//   - happy: 200 + file_id/file_path(uploads/ 前缀) + size_bytes 与输入一致
//   - 类型白名单: 非 .zip/.tar.gz/.tgz → 400
//   - 超限: >25MB → 400

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net"
	"net/http/httptest"
	"strconv"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/codeaudit/proto-gen"
)

// fakeStorageUpload — 捕获 UploadFile 流的 storage fake（仅本文件使用）。
type fakeStorageUpload struct {
	pb.UnimplementedStorageServiceServer
	gotPath  string
	gotBytes []byte
}

func (f *fakeStorageUpload) UploadFile(stream grpc.ClientStreamingServer[pb.UploadFileChunk, pb.StoredFile]) error {
	var buf []byte
	var path, ct string
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if chunk.GetFirstChunk() {
			path, ct = chunk.GetFilePath(), chunk.GetContentType()
		}
		buf = append(buf, chunk.GetData()...)
	}
	f.gotPath, f.gotBytes = path, buf
	_ = ct
	return stream.SendAndClose(&pb.StoredFile{
		FileId:    "file-test",
		FilePath:  path,
		SizeBytes: int64(len(buf)),
	})
}

func (f *fakeStorageUpload) DownloadFile(req *pb.DownloadFileRequest, s pb.StorageService_DownloadFileServer) error {
	return nil
}
func (f *fakeStorageUpload) GetPresignedUrl(ctx context.Context, r *pb.GetPresignedUrlRequest) (*pb.GetPresignedUrlResponse, error) {
	return nil, nil
}
func (f *fakeStorageUpload) GetFileInfo(ctx context.Context, r *pb.GetFileInfoRequest) (*pb.StoredFile, error) {
	return &pb.StoredFile{FileId: r.GetFileId(), FilePath: f.gotPath}, nil
}
func (f *fakeStorageUpload) DeleteFile(ctx context.Context, r *pb.DeleteFileRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}
func (f *fakeStorageUpload) ListFiles(ctx context.Context, r *pb.ListFilesRequest) (*pb.ListFilesResponse, error) {
	return &pb.ListFilesResponse{}, nil
}

// newUploadTranscoder — 起一个 fake storage 后端并接进 Transcoder。
func newUploadTranscoder(t *testing.T, fake *fakeStorageUpload) *Transcoder {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := grpc.NewServer()
	pb.RegisterStorageServiceServer(s, fake)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	tr := NewTranscoder(BackendAddrs{
		StorageAddr:  lis.Addr().String(),
		CallTimeoutS: 5,
	})
	t.Cleanup(tr.Close)
	return tr
}

func postArchive(t *testing.T, tr *Transcoder, filename string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", filename)
	fw.Write(content)
	mw.Close()
	req := httptest.NewRequest("POST", "/v1/uploads/archive", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	tr.UploadArchive(rec, req)
	return rec
}

func TestUploadArchive_StreamedToStorage(t *testing.T) {
	fake := &fakeStorageUpload{}
	tr := newUploadTranscoder(t, fake)
	content := []byte("PK-fake-archive-content")
	rec := postArchive(t, tr, "proj.zip", content)
	if rec.Code != 200 {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if fake.gotPath == "" || len(fake.gotBytes) != len(content) {
		t.Fatalf("storage must receive full stream: path=%q bytes=%d", fake.gotPath, len(fake.gotBytes))
	}
	if !bytes.HasPrefix([]byte(fake.gotPath), []byte("uploads/")) {
		t.Fatalf("object path must be uploads/-prefixed, got %q", fake.gotPath)
	}
	body := rec.Body.String()
	for _, key := range []string{`"file_id"`, `"upload_id"`, `"file_path":"uploads/`, `"size_bytes":` + strconv.Itoa(len(content))} {
		if !bytes.Contains([]byte(body), []byte(key)) {
			t.Fatalf("response missing %s: %s", key, body)
		}
	}
}

func TestUploadArchive_TypeRejected(t *testing.T) {
	tr := newUploadTranscoder(t, &fakeStorageUpload{})
	rec := postArchive(t, tr, "evil.exe", []byte("MZ..."))
	if rec.Code != 400 {
		t.Fatalf("non-archive must be rejected: got %d", rec.Code)
	}
}

func TestUploadArchive_OversizeRejected(t *testing.T) {
	tr := newUploadTranscoder(t, &fakeStorageUpload{})
	rec := postArchive(t, tr, "big.zip", bytes.Repeat([]byte("a"), uploadMaxBytes+1))
	if rec.Code != 400 {
		t.Fatalf("oversize must be rejected: got %d", rec.Code)
	}
}
