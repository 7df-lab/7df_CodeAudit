package service

// ADR-200 回归锁：上传件从 storage 拉回并解包（archive.go）。
// R33（gw-e295b637 实证）：落盘名用 filepath.Ext 取扩展名——.tar.gz 是双段后缀
// 只取到 .gz，自产的 archive-<ts>.gz 不满足自家解包 switch → .tar.gz 上传任务
// prepare 必挂；.zip/.tgz 单段后缀幸存，故 ADR-200 起 E2E（zip 实测）未暴露。

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/codeaudit/proto-gen"
)

// fakeStorageFetch — 返回登记的 file_path 并回放登记字节的 storage fake（仅本文件）。
type fakeStorageFetch struct {
	pb.UnimplementedStorageServiceServer
	filePath string
	content  []byte
}

func (f *fakeStorageFetch) GetFileInfo(ctx context.Context, r *pb.GetFileInfoRequest) (*pb.StoredFile, error) {
	return &pb.StoredFile{FileId: r.GetFileId(), FilePath: f.filePath, SizeBytes: int64(len(f.content))}, nil
}

func (f *fakeStorageFetch) DownloadFile(req *pb.DownloadFileRequest, s pb.StorageService_DownloadFileServer) error {
	for i := 0; i < len(f.content); i += 64 << 10 {
		end := i + 64<<10
		if end > len(f.content) {
			end = len(f.content)
		}
		if err := s.Send(&pb.DownloadFileChunk{Data: f.content[i:end]}); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeStorageFetch) UploadFile(stream grpc.ClientStreamingServer[pb.UploadFileChunk, pb.StoredFile]) error {
	return io.EOF
}
func (f *fakeStorageFetch) GetPresignedUrl(ctx context.Context, r *pb.GetPresignedUrlRequest) (*pb.GetPresignedUrlResponse, error) {
	return nil, nil
}
func (f *fakeStorageFetch) DeleteFile(ctx context.Context, r *pb.DeleteFileRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}
func (f *fakeStorageFetch) ListFiles(ctx context.Context, r *pb.ListFilesRequest) (*pb.ListFilesResponse, error) {
	return &pb.ListFilesResponse{}, nil
}

// newFetchTarget — 起一个 fake storage gRPC 端点供 FetchUploadArchive 拨号。
func newFetchTarget(t *testing.T, fake *fakeStorageFetch) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := grpc.NewServer()
	pb.RegisterStorageServiceServer(s, fake)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	return lis.Addr().String()
}

func mustTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), ModTime: time.Now()}); err != nil {
			t.Fatalf("tar header: %v", err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar body: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func mustZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// TestFetchUploadArchive_TarGzFullChain — R33 锁：.tar.gz 上传件全链（元数据→
// 落盘名→解包 switch）必须以完整双段后缀贯通。变异面 M29：archiveExt 退回
// filepath.Ext 即红（复现 gw-e295b637 "不支持的格式: archive-<ts>.gz"）。
func TestFetchUploadArchive_TarGzFullChain(t *testing.T) {
	files := map[string]string{"repo/main.py": "import os\n"}
	addr := newFetchTarget(t, &fakeStorageFetch{filePath: "uploads/repo.tar.gz", content: mustTarGz(t, files)})
	root, err := FetchUploadArchive(context.Background(), addr, "file-1", t.TempDir())
	if err != nil {
		t.Fatalf(".tar.gz 上传件 prepare 必须可解包（gw-e295b637 回归）: %v", err)
	}
	if !strings.HasSuffix(root, "repo") {
		t.Fatalf("解包根应剥壳降入 repo/，got %q", root)
	}
	body, err := os.ReadFile(root + "/main.py")
	if err != nil || string(body) != "import os\n" {
		t.Fatalf("解包内容不符: %v %q", err, body)
	}
}

// TestFetchUploadArchive_TgzAndZip — 单段后缀两档在命名归一后不得回退。
func TestFetchUploadArchive_TgzAndZip(t *testing.T) {
	tgz := newFetchTarget(t, &fakeStorageFetch{filePath: "uploads/repo.tgz", content: mustTarGz(t, map[string]string{"r/a.txt": "x"})})
	if _, err := FetchUploadArchive(context.Background(), tgz, "f", t.TempDir()); err != nil {
		t.Fatalf(".tgz 回退: %v", err)
	}
	zipAddr := newFetchTarget(t, &fakeStorageFetch{filePath: "uploads/repo.zip", content: mustZip(t, map[string]string{"r/a.txt": "x"})})
	if _, err := FetchUploadArchive(context.Background(), zipAddr, "f", t.TempDir()); err != nil {
		t.Fatalf(".zip 回退: %v", err)
	}
}

// TestFetchUploadArchive_UnsupportedExt — 白名单语义保持：裸 .gz 拒收。
func TestFetchUploadArchive_UnsupportedExt(t *testing.T) {
	addr := newFetchTarget(t, &fakeStorageFetch{filePath: "uploads/dump.gz", content: []byte{0x1f, 0x8b}})
	_, err := FetchUploadArchive(context.Background(), addr, "f", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "不支持的格式") {
		t.Fatalf("裸 .gz 应被白名单拒收，got %v", err)
	}
}
