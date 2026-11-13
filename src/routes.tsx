import { Navigate, Route, Routes } from 'react-router-dom';
import CasePage from './pages/CasePage';
import AnalysisPage from './pages/AnalysisPage';
import RecoveryPage from './pages/RecoveryPage';
import TimelinePage from './pages/TimelinePage';
import ReportPage from './pages/ReportPage';

export default function AppRoutes() {
  return (
    <Routes>
      <Route path="/" element={<Navigate to="/case" replace />} />
      <Route path="/case" element={<CasePage />} />
      <Route path="/analysis" element={<AnalysisPage />} />
      <Route path="/recovery" element={<RecoveryPage />} />
      <Route path="/timeline" element={<TimelinePage />} />
      <Route path="/report" element={<ReportPage />} />
      <Route path="*" element={<Navigate to="/case" replace />} />
    </Routes>
  );
}
