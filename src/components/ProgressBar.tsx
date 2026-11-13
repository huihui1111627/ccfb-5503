import type { ProgressPayload } from '../workers/analysis.worker';

export function ProgressBar({ progress }: { progress: ProgressPayload }) {
  const pct = progress.total && progress.total > 0
    ? Math.min(100, Math.round((progress.current ?? 0) / progress.total * 100))
    : null;
  return (
    <div className="progress-wrap" role="status" aria-live="polite">
      <div className="progress-head">
        <span>{progress.phase}</span>
        {progress.message && <span className="muted">{progress.message}</span>}
        {pct !== null && <span className="mono">{pct}%</span>}
      </div>
      <div className="progress-track">
        <div
          className={`progress-fill ${pct === null ? 'indeterminate' : ''}`}
          style={pct === null ? undefined : { width: `${pct}%` }}
        />
      </div>
    </div>
  );
}
