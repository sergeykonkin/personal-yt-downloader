import { describe, expect, it } from 'vitest';

import type { JobView } from './api';
import { hasPercent, inFlight, percent, stageLabel } from './stages';

function job(partial: Partial<JobView>): JobView {
  return { id: 'x', title: 'T', status: 'queued', stage: 'queued', size_bytes: 0, created_at: '2026-01-01T00:00:00Z', ...partial };
}

describe('stageLabel', () => {
  it('labels every stage distinctly', () => {
    expect(stageLabel(job({ status: 'queued', stage: 'queued' }))).toBe('Queued');
    expect(stageLabel(job({ status: 'downloading', stage: 'downloading.video' }))).toBe('Downloading video');
    expect(stageLabel(job({ status: 'downloading', stage: 'downloading.audio' }))).toBe('Downloading audio');
    expect(stageLabel(job({ status: 'processing', stage: 'processing' }))).toBe('Processing');
    expect(stageLabel(job({ status: 'verifying', stage: 'verifying' }))).toBe('Verifying');
    expect(stageLabel(job({ status: 'ready', stage: 'ready' }))).toBe('Ready');
    expect(stageLabel(job({ status: 'failed', stage: 'failed', error: 'x' }))).toBe('Failed');
    expect(stageLabel(job({ status: 'queued', stage: 'unknown-stage' }))).toBe('Queued');
  });
});

describe('progress helpers', () => {
  it('flags in-flight statuses', () => {
    expect(inFlight(job({ status: 'downloading' }))).toBe(true);
    expect(inFlight(job({ status: 'processing' }))).toBe(true);
    expect(inFlight(job({ status: 'verifying' }))).toBe(true);
    expect(inFlight(job({ status: 'queued' }))).toBe(false);
    expect(inFlight(job({ status: 'ready' }))).toBe(false);
    expect(inFlight(job({ status: 'failed' }))).toBe(false);
  });

  it('detects and clamps percentages', () => {
    expect(hasPercent(job({ progress: 42 }))).toBe(true);
    expect(hasPercent(job({ progress: null }))).toBe(false);
    expect(hasPercent(job({}))).toBe(false);
    expect(percent(job({ progress: 42 }))).toBe(42);
    expect(percent(job({ progress: 150 }))).toBe(100);
    expect(percent(job({ progress: -3 }))).toBe(0);
  });
});
