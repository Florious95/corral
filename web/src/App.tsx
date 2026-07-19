import { BrowserRouter, Navigate, Route, Routes, useParams } from 'react-router-dom';
import { ConsoleLayout } from './components/ConsoleLayout';
import { useDesktopLayout } from './hooks/useDesktopLayout';
import { DesktopConsole } from './pages/DesktopConsole';
import { SessionTimeline } from './pages/SessionTimeline';
import { Sessions } from './pages/Sessions';
import { V2LiveConsole, V2LiveTimeline } from './v2/V2LiveConsole';
import { V2MockConsole, V2MockTimeline } from './v2/V2MockConsole';

function v2DebugMode(): 'mock' | 'live' | null {
  const mode = new URLSearchParams(location.search).get('data');
  if (mode === 'v2debugmock') return 'mock';
  if (mode === 'v2debug') return 'live';
  return null;
}

function Home() {
  const desktop = useDesktopLayout();
  if (v2DebugMode() === 'mock') return <V2MockConsole />;
  if (v2DebugMode() === 'live') return <V2LiveConsole />;
  return desktop ? <DesktopConsole /> : <ConsoleLayout><Sessions /></ConsoleLayout>;
}

function SessionRoute() {
  const desktop = useDesktopLayout();
  const { hostId, id } = useParams();
  return desktop ? <DesktopConsole /> : <SessionTimeline key={`${hostId}:${id}`} />;
}

function V2Route({ kind }: { kind: 'entry' | 'record' }) {
  const mode = v2DebugMode();
  if (!mode) return <Navigate to={{ pathname: '/', search: location.search }} replace />;
  return mode === 'mock' ? <V2MockTimeline kind={kind} /> : <V2LiveTimeline kind={kind} />;
}

export default function App() {
  return (
    <BrowserRouter>
      <Routes>
        <Route path="/" element={<Home />} />
        <Route path="/sessions/:hostId/:id" element={<SessionRoute />} />
        <Route path="/v2/entries/:hostId/:entryId" element={<V2Route kind="entry" />} />
        <Route path="/v2/records/:hostId/:recordId" element={<V2Route kind="record" />} />
      </Routes>
    </BrowserRouter>
  );
}
