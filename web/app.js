// TeleClone Pro Web Client (Zero-Dependency Vanilla JS)
document.addEventListener('DOMContentLoaded', () => {
  let activeSnapshots = [];
  let currentSnapshotID = '';
  let currentFolderPath = '';

  // Tab Switching
  document.querySelectorAll('.nav-tab').forEach(tab => {
    tab.addEventListener('click', () => {
      document.querySelectorAll('.nav-tab').forEach(t => t.classList.remove('active'));
      document.querySelectorAll('.content-section').forEach(s => s.classList.remove('active'));

      tab.classList.add('active');
      const targetSection = document.getElementById(`section-${tab.dataset.tab}`);
      if (targetSection) targetSection.classList.add('active');

      if (tab.dataset.tab === 'filestation') {
        loadLatestFileStation();
      } else if (tab.dataset.tab === 'timemachine') {
        loadTimeMachine();
      }
    });
  });

  // Modal Handlers
  const modal = document.getElementById('restoreModal');
  const closeModal = () => modal.classList.remove('open');
  document.getElementById('btnCloseModal').addEventListener('click', closeModal);
  document.getElementById('btnCancelRestore').addEventListener('click', closeModal);

  window.openRestoreModal = (snapID, path) => {
    document.getElementById('modalSnapID').value = snapID;
    document.getElementById('modalTargetPath').value = path;
    modal.classList.add('open');
  };

  document.getElementById('btnConfirmRestore').addEventListener('click', async () => {
    const snapID = document.getElementById('modalSnapID').value;
    const path = document.getElementById('modalTargetPath').value;
    const dest = document.getElementById('modalDestPath').value;

    try {
      const res = await fetch('/api/restore', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ snapshot_id: snapID, path: path, target_dir: dest })
      });
      if (!res.ok) {
        alert('Restore failed: ' + (await res.text()));
        return;
      }
      closeModal();
      alert('Restore task initiated in background! Check Overview tab for progress.');
    } catch (err) {
      alert('Restore error: ' + err.message);
    }
  });

  // Trigger Backup
  document.getElementById('btnTriggerBackup').addEventListener('click', async () => {
    try {
      const res = await fetch('/api/backup/trigger', { method: 'POST' });
      if (!res.ok) {
        alert('Backup trigger failed: ' + (await res.text()));
        return;
      }
      refreshStatus();
    } catch (err) {
      alert('Error: ' + err.message);
    }
  });

  // Load Status & Overview Metrics
  async function refreshStatus() {
    try {
      const res = await fetch('/api/status');
      if (!res.ok) return;
      const data = await res.json();

      document.getElementById('headerNodeBadge').innerText = `${data.node_name} (${data.node_mode})`;
      document.getElementById('valTotalStorage').innerText = formatBytes(data.total_storage_bytes);
      document.getElementById('valStorageSub').innerText = `Source: ${data.source_path}`;
      document.getElementById('valTotalSnapshots').innerText = data.total_snapshots;
      document.getElementById('valLatestSnapshotTime').innerText = data.latest_snapshot_at ? `Latest: ${new Date(data.latest_snapshot_at).toLocaleString()}` : 'Latest: None';
      document.getElementById('valBotCount').innerText = `${data.bot_mesh_count} Active Bots`;
      document.getElementById('valActiveBandwidth').innerText = data.active_bandwidth > 0 ? `${formatBytes(data.active_bandwidth)}/s` : 'Unlimited';

      // Update Live Job State
      const chip = document.getElementById('chipJobStatus');
      const heading = document.getElementById('textJobHeading');
      const speed = document.getElementById('textJobSpeed');
      const bar = document.getElementById('barJobProgress');
      const details = document.getElementById('textJobDetails');
      const percent = document.getElementById('textJobPercent');

      if (data.is_backing_up) {
        chip.className = 'chip chip-warning';
        chip.innerText = 'Backing Up';
        heading.innerText = 'Creating point-in-time snapshot...';
        if (data.last_backup_event) {
          const ev = data.last_backup_event;
          bar.style.width = `${ev.percent_complete}%`;
          percent.innerText = `${ev.percent_complete.toFixed(1)}%`;
          details.innerText = `[${ev.processed_files}/${ev.total_files}] Delta: ${formatBytes(ev.uploaded_bytes)} | ${ev.current_file}`;
          speed.innerText = ev.current_speed_bps > 0 ? `${formatBytes(ev.current_speed_bps)}/s` : 'Throttled';
        }
      } else if (data.is_restoring) {
        chip.className = 'chip chip-info';
        chip.innerText = 'Restoring';
        heading.innerText = 'Restoring files from Telegram...';
        if (data.last_restore_event) {
          const ev = data.last_restore_event;
          bar.style.width = `${ev.percent}%`;
          percent.innerText = `${ev.percent.toFixed(1)}%`;
          details.innerText = `Restored ${ev.restored_files}/${ev.total_files} files: ${ev.current_file}`;
        }
      } else {
        chip.className = 'chip chip-success';
        chip.innerText = 'Idle';
        heading.innerText = 'System is ready (0 MB RAM overhead)';
        bar.style.width = '0%';
        percent.innerText = '0%';
        speed.innerText = '0.0 MB/s';
        details.innerText = 'Next run scheduled via cron or click Backup Now';
      }
    } catch (err) {
      console.error('Status fetch error:', err);
    }
  }

  // Load Bot Mesh
  async function refreshBots() {
    try {
      const res = await fetch('/api/bots');
      if (!res.ok) return;
      const bots = await res.json();
      const tbody = document.getElementById('botTableBody');

      if (!bots || bots.length === 0) {
        tbody.innerHTML = '<tr><td colspan="5" style="text-align: center; color: var(--text-dim);">No bots configured in mesh.</td></tr>';
        return;
      }

      tbody.innerHTML = bots.map(b => `
        <tr>
          <td><code style="font-family: var(--font-mono);">${b.token_prefix}</code></td>
          <td><span class="chip ${b.is_available ? 'chip-success' : 'chip-danger'}">${b.is_available ? 'Ready' : 'Backoff'}</span></td>
          <td>${b.requests_sent}</td>
          <td>${formatBytes(b.bytes_sent)}</td>
          <td>${b.last_error ? `<span style="color: var(--danger); font-size: 11px;">${b.last_error}</span>` : '<span style="color: var(--text-dim); font-size: 11px;">Nominal</span>'}</td>
        </tr>
      `).join('');
    } catch (err) {
      console.error('Bots fetch error:', err);
    }
  }

  // File Station
  async function loadLatestFileStation() {
    try {
      const res = await fetch('/api/snapshots');
      if (!res.ok) return;
      activeSnapshots = await res.json();
      if (activeSnapshots.length === 0) {
        document.getElementById('fsTableBody').innerHTML = '<tr><td colspan="6" style="text-align: center; color: var(--text-dim);">No snapshots exist yet. Run backup first.</td></tr>';
        return;
      }
      currentSnapshotID = activeSnapshots[0].snapshot_id;
      loadDirectory(currentSnapshotID, '');
    } catch (err) {
      console.error('File station load error:', err);
    }
  }

  async function loadDirectory(snapID, path) {
    currentFolderPath = path;
    document.getElementById('fsBreadcrumb').innerText = path ? `/${path}` : '/';

    try {
      const res = await fetch(`/api/snapshots/${snapID}/tree?path=${encodeURIComponent(path)}`);
      if (!res.ok) return;
      const data = await res.json();
      const tbody = document.getElementById('fsTableBody');

      if (!data.items || data.items.length === 0) {
        tbody.innerHTML = '<tr><td colspan="6" style="text-align: center; color: var(--text-dim);">This folder is empty.</td></tr>';
        return;
      }

      tbody.innerHTML = data.items.map(item => `
        <tr>
          <td>
            <div style="display: flex; align-items: center; gap: 8px;">
              <svg class="icon" viewBox="0 0 24 24" style="color: ${item.is_dir ? '#58a6ff' : 'var(--text-muted)'};">
                ${item.is_dir ? '<path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"></path>' : '<path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"></path><polyline points="14 2 14 8 20 8"></polyline>'}
              </svg>
              ${item.is_dir ? `<a href="javascript:void(0)" onclick="window.fsNavigate('${item.path}')" style="color: var(--text-main); text-decoration: none; font-weight: 500;">${item.name}</a>` : `<span>${item.name}</span>`}
            </div>
          </td>
          <td>${item.is_dir ? '-' : formatBytes(item.size)}</td>
          <td style="color: var(--text-muted); font-size: 11px;">${new Date(item.mod_time).toLocaleString()}</td>
          <td>${item.is_dir ? '-' : (item.chunk_count || 1)}</td>
          <td><span class="chip chip-success">Protected</span></td>
          <td style="text-align: right;">
            <button class="btn btn-sm btn-accent" onclick="window.openRestoreModal('${snapID}', '${item.path}')">
              Restore
            </button>
          </td>
        </tr>
      `).join('');
    } catch (err) {
      console.error('Directory read error:', err);
    }
  }

  window.fsNavigate = (path) => {
    loadDirectory(currentSnapshotID, path);
  };

  document.getElementById('btnRestoreCurrentFolder').addEventListener('click', () => {
    if (!currentSnapshotID) return;
    openRestoreModal(currentSnapshotID, currentFolderPath);
  });

  // Time Machine Timeline
  async function loadTimeMachine() {
    try {
      const res = await fetch('/api/snapshots');
      if (!res.ok) return;
      activeSnapshots = await res.json();
      const scrubber = document.getElementById('tmScrubber');

      if (!activeSnapshots || activeSnapshots.length === 0) {
        scrubber.innerHTML = '<div style="color: var(--text-dim); padding: 10px;">No snapshots available.</div>';
        return;
      }

      scrubber.innerHTML = activeSnapshots.map((s, idx) => `
        <div class="snap-node ${idx === 0 ? 'active' : ''}" onclick="window.selectSnapshot('${s.snapshot_id}', this)">
          <div class="snap-time">${new Date(s.created_at).toLocaleDateString()} ${new Date(s.created_at).toLocaleTimeString([], {hour: '2-digit', minute:'2-digit'})}</div>
          <div class="snap-id">${s.snapshot_id}</div>
          <div class="snap-stats">${s.total_files} files | ${formatBytes(s.total_bytes)}</div>
        </div>
      `).join('');

      selectSnapshot(activeSnapshots[0].snapshot_id);
    } catch (err) {
      console.error('Time machine load error:', err);
    }
  }

  window.selectSnapshot = async (snapID, el) => {
    if (el) {
      document.querySelectorAll('.snap-node').forEach(n => n.classList.remove('active'));
      el.classList.add('active');
    }

    document.getElementById('tmDiffTitle').innerText = `Snapshot Manifest: ${snapID}`;
    try {
      const res = await fetch(`/api/snapshots/${snapID}`);
      if (!res.ok) return;
      const manifest = await res.json();
      const tbody = document.getElementById('tmDiffTableBody');

      // Check if there is a previous snapshot to diff against
      const currentIdx = activeSnapshots.findIndex(s => s.snapshot_id === snapID);
      let diffMap = {};
      let addedCount = 0, modifiedCount = 0, deletedCount = 0;

      if (currentIdx !== -1 && currentIdx + 1 < activeSnapshots.length) {
        const prevID = activeSnapshots[currentIdx + 1].snapshot_id;
        const diffRes = await fetch(`/api/snapshots/diff?a=${encodeURIComponent(prevID)}&b=${encodeURIComponent(snapID)}`);
        if (diffRes.ok) {
          const diffData = await diffRes.json();
          (diffData.diffs || []).forEach(d => {
            diffMap[d.path] = d.status;
            if (d.status === 'ADDED') addedCount++;
            if (d.status === 'MODIFIED') modifiedCount++;
            if (d.status === 'DELETED') deletedCount++;
          });
        }
      }

      document.getElementById('chipDiffAdded').innerText = `${addedCount} Added`;
      document.getElementById('chipDiffModified').innerText = `${modifiedCount} Modified`;
      document.getElementById('chipDiffDeleted').innerText = `${deletedCount} Deleted`;

      const files = Object.values(manifest.files || {});
      if (files.length === 0) {
        tbody.innerHTML = '<tr><td colspan="5" style="text-align: center; color: var(--text-dim);">No files in this snapshot.</td></tr>';
        return;
      }

      tbody.innerHTML = files.map(f => {
        const status = diffMap[f.path] || 'UNCHANGED';
        let chipClass = 'chip-success';
        let chipText = 'Synced';
        if (status === 'ADDED') {
          chipClass = 'chip-success';
          chipText = 'Added';
        } else if (status === 'MODIFIED') {
          chipClass = 'chip-warning';
          chipText = 'Modified';
        }

        return `
          <tr>
            <td><span class="chip ${chipClass}">${chipText}</span></td>
            <td style="font-family: var(--font-mono);">${f.path}</td>
            <td>${formatBytes(f.size)}</td>
            <td><code style="font-size: 10px; color: var(--text-muted);">${(f.file_hash || '').substring(0, 16)}...</code></td>
            <td style="text-align: right;">
              <button class="btn btn-sm btn-accent" onclick="window.openRestoreModal('${snapID}', '${f.path}')">Restore</button>
            </td>
          </tr>
        `;
      }).join('');
    } catch (err) {
      console.error('Snapshot detail fetch error:', err);
    }
  };

  // Settings Save Handlers
  document.getElementById('btnSaveTopology').addEventListener('click', async () => {
    const payload = {
      node_name: document.getElementById('cfgNodeName').value,
      node_mode: document.getElementById('cfgNodeMode').value,
      source_path: document.getElementById('cfgSourcePath').value
    };
    try {
      const res = await fetch('/api/config', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload)
      });
      if (res.ok) alert('Topology saved successfully!');
    } catch (err) {
      alert('Save error: ' + err.message);
    }
  });

  document.getElementById('btnSaveQoS').addEventListener('click', async () => {
    const payload = {
      bandwidth_schedule: document.getElementById('cfgBwSchedule').value,
      retention: {
        keep_hourly: parseInt(document.getElementById('cfgKeepHourly').value, 10),
        keep_daily: parseInt(document.getElementById('cfgKeepDaily').value, 10),
        keep_weekly: parseInt(document.getElementById('cfgKeepWeekly').value, 10),
        keep_monthly: parseInt(document.getElementById('cfgKeepMonthly').value, 10)
      }
    };
    try {
      const res = await fetch('/api/config', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload)
      });
      if (res.ok) alert('QoS & Retention saved successfully!');
    } catch (err) {
      alert('Save error: ' + err.message);
    }
  });

  // SSE Real-Time Stream
  function initSSE() {
    const es = new EventSource('/api/events');
    es.addEventListener('backup_progress', (e) => {
      const ev = JSON.parse(e.data);
      refreshStatus();
    });
    es.addEventListener('backup_complete', () => {
      refreshStatus();
      refreshBots();
      loadLatestFileStation();
    });
    es.addEventListener('restore_progress', (e) => {
      refreshStatus();
    });
    es.addEventListener('restore_complete', () => {
      refreshStatus();
      alert('File restoration completed successfully!');
    });
    es.onerror = () => {
      // Fallback polling if disconnected
      setTimeout(refreshStatus, 3000);
    };
  }

  // Helper
  function formatBytes(bytes) {
    if (bytes === 0 || !bytes) return '0 B';
    const k = 1024;
    const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
    const i = Math.floor(Math.log(bytes) / Math.log(k));
    return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i];
  }

  // Initial Boot
  refreshStatus();
  refreshBots();
  initSSE();
  setInterval(refreshStatus, 5000);
});
