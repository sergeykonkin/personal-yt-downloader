// Stage and status presentation shared by cards and tests.

import type { JobView } from './api';

export function stageLabel(job: JobView): string {
  switch (job.status) {
    case 'ready':
      return 'Ready';
    case 'failed':
      return 'Failed';
    case 'downloading':
      return stageDetail(job);
    default:
      return stageDetail(job);
  }
}

function stageDetail(job: JobView): string {
  switch (job.stage) {
    case 'downloading.video':
      return 'Downloading video';
    case 'downloading.audio':
      return 'Downloading audio';
    case 'processing':
      return 'Processing';
    case 'verifying':
      return 'Verifying';
    case 'ready':
      return 'Ready';
    case 'failed':
      return 'Failed';
    default:
      return 'Queued';
  }
}

// A job is in flight when it shows a progress bar.
export function inFlight(job: JobView): boolean {
  return job.status === 'downloading' || job.status === 'processing' || job.status === 'verifying';
}

export function hasPercent(job: JobView): boolean {
  return typeof job.progress === 'number' && Number.isFinite(job.progress);
}

export function percent(job: JobView): number {
  const value = typeof job.progress === 'number' ? job.progress : 0;
  return Math.min(100, Math.max(0, value));
}
