// /projects/new 薄壳宿主页——与 /tasks/new 同构：弹窗即页面
// 内容，关闭回项目列表。此前总览"新建项目"走 /projects?new=1 参数式深链（就地开弹窗+
// 剥参），与任务侧 /tasks/new?project_id= 参数化薄壳是两套范式；统一为 <资源>/new。
// 旧链 ?new=1 由 ProjectsPage 兼容重定向至此（保书签/外链）。
import { useNavigate } from 'react-router-dom';
import { ProjectCreateModal } from '../components/ProjectCreateModal';
import { usePageTitle } from '../hooks/usePageTitle';

export default function ProjectNewPage() {
  usePageTitle('新建项目');
  const navigate = useNavigate();
  return <ProjectCreateModal open onClose={() => navigate('/projects')} />;
}
