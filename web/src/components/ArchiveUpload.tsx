// 代码压缩包上传交互件（ADR-203 零落盘直传的共享面）：multipart 经 gateway 流式转发
// storage（MinIO），回执 file_id 交宿主落项目 config（创建弹窗/配置源码弹窗同契约）。
// 原为 ProjectsPage 新建弹窗内联实现；项目详情"配置源码来源"弹窗（ProjectSourceModal）
// 复用同一交互——抽件防兄弟漂移（交互审计 Top4/B4-2 的受控清空语义一并随件走）。
// 受控值对象单一状态源：fileId+fileName 由宿主持有（提交消费/重开回收），上传中自锁；
// 宿主重置 fileId 为空时组件同步清 chip（B4-2：取消后再开不残留旧上传态）。
import { InboxOutlined } from '@ant-design/icons';
import { Upload, message, type UploadFile } from 'antd';
import { useEffect, useState } from 'react';
import { MAX_ARCHIVE_UPLOAD_BYTES, uploadArchive } from '../api/client';

export interface ArchiveUploadValue {
  fileId: string | null;
  fileName: string;
}

export function ArchiveUploadDragger({ value, onChange }: {
  value: ArchiveUploadValue;
  onChange: (v: ArchiveUploadValue) => void;
}) {
  const [uploading, setUploading] = useState(false);
  const [fileList, setFileList] = useState<UploadFile[]>([]);
  // B4-2 语义随件：宿主回收 value（fileId 置空）即清受控 chip，弹窗重开不残留旧文件名
  useEffect(() => {
    if (!value.fileId) setFileList([]);
  }, [value.fileId]);
  return (
    <Upload.Dragger
      accept=".zip,.tgz,.tar.gz"
      maxCount={1}
      fileList={fileList}
      disabled={uploading}
      beforeUpload={async (file) => {
        // B5(P3-i)：扩展名预检——accept 只约束文件选择器，拖拽可绕过；服务端 400 详情
        // 此前也不透出（uploadArchive 未提取 {error}），前端拦截给出即时归因
        if (!/\.(zip|tgz|tar\.gz)$/i.test(file.name) || file.size > MAX_ARCHIVE_UPLOAD_BYTES) {
          message.error('上传失败（仅支持 zip/tar.gz，≤100MB）');
          return Upload.LIST_IGNORE; // 含 100MB 本地预检：超限不发起请求（nginx 413 兜底）
        }
        setUploading(true);
        try {
          const res = await uploadArchive(file);
          onChange({ fileId: res.file_id, fileName: file.name });
          setFileList([{ uid: res.file_id, name: file.name, status: 'done' }]);
          message.success('上传成功——项目将以该压缩包为源码');
        } catch (e) {
          message.error(`上传失败：${(e as Error).message}`);
        } finally {
          setUploading(false);
        }
        return Upload.LIST_IGNORE; // 受控 fileList，禁 antd 自行追加
      }}
      onRemove={() => {
        setFileList([]);
        onChange({ fileId: null, fileName: '' });
      }}
    >
      <p className="ant-upload-drag-icon"><InboxOutlined /></p>
      <p className="ant-upload-text">点击或拖拽上传压缩包</p>
      <p className="ant-upload-hint">扫描启动时自动拉取并解包，无需手动处理</p>
    </Upload.Dragger>
  );
}
