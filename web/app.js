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
      } else if (tab.dataset.tab === 'settings') {
        loadSettings();
      }
    });
  });

  // Modal Handlers
  const modal = document.getElementById('restoreModal');
  const closeModal = () => modal.classList.remove('open');
  document.getElementById('btnCloseModal').addEventListener('click', closeModal);
  document.getElementById('btnCancelRestore').addEventListener('click', closeModal);

  // Destination Presets
  const btnPresetRestoreDir = document.getElementById('btnPresetRestoreDir');
  const btnPresetOriginalDir = document.getElementById('btnPresetOriginalDir');
  if (btnPresetRestoreDir) {
    btnPresetRestoreDir.addEventListener('click', () => {
      document.getElementById('modalDestPath').value = '/volume1/restored_data';
    });
  }
  if (btnPresetOriginalDir) {
    btnPresetOriginalDir.addEventListener('click', () => {
      document.getElementById('modalDestPath').value = window.cfgSourcePath || '/volume1/data';
    });
  }

  window.openRestoreModal = async (targetSnapID, path) => {
    const snapSelect = document.getElementById('modalSnapSelect');
    snapSelect.innerHTML = '';

    // Ensure activeSnapshots is loaded
    if (!activeSnapshots || activeSnapshots.length === 0) {
      try {
        const res = await fetch('/api/snapshots');
        if (res.ok) activeSnapshots = await res.json();
      } catch (e) {}
    }

    if (activeSnapshots && activeSnapshots.length > 0) {
      snapSelect.innerHTML = activeSnapshots.map(s => {
        const d = new Date(s.created_at);
        const dateStr = d.toLocaleDateString() + ' ' + d.toLocaleTimeString([], {hour: '2-digit', minute:'2-digit', second:'2-digit'});
        return `<option value="${s.snapshot_id}">${dateStr} &bull; ${s.snapshot_id} (${s.total_files} files, ${formatBytes(s.total_bytes)})</option>`;
      }).join('');
    } else {
      snapSelect.innerHTML = `<option value="${targetSnapID || ''}">${targetSnapID || 'No snapshots found'}</option>`;
    }

    if (targetSnapID) {
      snapSelect.value = targetSnapID;
    }

    document.getElementById('modalTargetPath').value = path || '/';

    const updateSnapMeta = () => {
      const selectedID = snapSelect.value;
      const found = (activeSnapshots || []).find(s => s.snapshot_id === selectedID);
      const metaEl = document.getElementById('modalSnapMeta');
      if (found) {
        metaEl.innerHTML = `<span>Mốc sao lưu: <strong>${new Date(found.created_at).toLocaleString()}</strong></span> <span>&bull;</span> <span>Dung lượng: ${formatBytes(found.total_bytes)}</span> <span>&bull;</span> <span>Delta: ${formatBytes(found.uploaded_bytes)}</span>`;
      } else {
        metaEl.innerHTML = `<span>Snapshot ID: <code>${selectedID}</code></span>`;
      }
    };
    snapSelect.onchange = updateSnapMeta;
    updateSnapMeta();

    // Check version history if path is a file (has extension)
    const vContainer = document.getElementById('modalVersionHistoryContainer');
    const vList = document.getElementById('modalVersionList');
    if (path && !path.endsWith('/') && path.includes('.')) {
      try {
        const res = await fetch(`/api/file/history?path=${encodeURIComponent(path)}`);
        if (res.ok) {
          const versions = await res.json();
          if (versions && versions.length > 0) {
            vContainer.style.display = 'block';
            vList.innerHTML = versions.map((v, idx) => {
              const isSelected = v.snapshot_id === snapSelect.value;
              const d = new Date(v.created_at);
              const dateStr = d.toLocaleDateString() + ' ' + d.toLocaleTimeString([], {hour: '2-digit', minute:'2-digit', second:'2-digit'});
              return `
                <div class="version-item" style="display: flex; justify-content: space-between; align-items: center; padding: 6px 10px; border-radius: 4px; border: 1px solid ${isSelected ? 'var(--info)' : 'var(--border-subtle)'}; background: ${isSelected ? 'rgba(88,166,255,0.1)' : 'var(--bg-surface)'};">
                  <div>
                    <div style="font-weight: 600; color: ${isSelected ? 'var(--info)' : 'var(--text-main)'}; font-size: 11px;">
                      Phiên bản ${versions.length - idx} ${idx === 0 ? '(Mới nhất)' : ''}
                    </div>
                    <div style="font-size: 11px; color: var(--text-muted); font-family: var(--font-mono);">
                      ${dateStr} &bull; ${formatBytes(v.size)} &bull; ${v.snapshot_id}
                    </div>
                  </div>
                  <button type="button" class="btn btn-sm ${isSelected ? 'btn-accent' : ''}" style="padding: 2px 8px; font-size: 11px;" onclick="window.selectRestoreVersion('${v.snapshot_id}', '${path}')">
                    ${isSelected ? 'Đang chọn' : 'Chọn mốc này'}
                  </button>
                </div>
              `;
            }).join('');
          } else {
            vContainer.style.display = 'none';
          }
        } else {
          vContainer.style.display = 'none';
        }
      } catch (e) {
        vContainer.style.display = 'none';
      }
    } else {
      vContainer.style.display = 'none';
    }

    modal.classList.add('open');
  };

  window.selectRestoreVersion = (snapID, path) => {
    const snapSelect = document.getElementById('modalSnapSelect');
    snapSelect.value = snapID;
    if (snapSelect.onchange) snapSelect.onchange();
    window.openRestoreModal(snapID, path);
  };

  document.getElementById('btnConfirmRestore').addEventListener('click', async () => {
    const snapID = document.getElementById('modalSnapSelect').value;
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
      alert(`Restore task for snapshot ${snapID} initiated! Files will be reassembled at ${dest}.`);
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
      const fsSelect = document.getElementById('fsSnapshotSelect');

      if (activeSnapshots.length === 0) {
        document.getElementById('fsTableBody').innerHTML = '<tr><td colspan="6" style="text-align: center; color: var(--text-dim);">No snapshots exist yet. Run backup first.</td></tr>';
        if (fsSelect) fsSelect.innerHTML = '<option value="">No snapshots found</option>';
        return;
      }

      if (fsSelect) {
        fsSelect.innerHTML = activeSnapshots.map(s => {
          const d = new Date(s.created_at);
          const dateStr = d.toLocaleDateString() + ' ' + d.toLocaleTimeString([], {hour: '2-digit', minute:'2-digit'});
          return `<option value="${s.snapshot_id}">${dateStr} &bull; ${s.snapshot_id}</option>`;
        }).join('');

        fsSelect.onchange = () => {
          currentSnapshotID = fsSelect.value;
          loadDirectory(currentSnapshotID, currentFolderPath);
        };
      }

      currentSnapshotID = activeSnapshots[0].snapshot_id;
      if (fsSelect) fsSelect.value = currentSnapshotID;
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

  // Settings & Credentials Manager
  let configuredBotTokens = [];

  async function loadSettings() {
    try {
      const res = await fetch('/api/config');
      if (!res.ok) return;
      const cfg = await res.json();

      document.getElementById('cfgChannelID').value = cfg.channel_id || '';
      document.getElementById('cfgNodeName').value = cfg.node_name || 'NAS-Node';
      document.getElementById('cfgNodeMode').value = cfg.node_mode || 'PUSH';
      document.getElementById('cfgSourcePath').value = cfg.source_path || '/volume1/data';
      document.getElementById('cfgBwSchedule').value = cfg.bandwidth_schedule || '07:00,5M 23:00,18M';
      if (document.getElementById('cfgChunkSize')) {
        document.getElementById('cfgChunkSize').value = cfg.chunk_size || 51380224;
      }
      if (document.getElementById('cfgExcludes') && cfg.excludes) {
        document.getElementById('cfgExcludes').value = cfg.excludes.join(', ');
      }
      if (cfg.retention) {
        document.getElementById('cfgKeepHourly').value = cfg.retention.keep_hourly || 24;
        document.getElementById('cfgKeepDaily').value = cfg.retention.keep_daily || 7;
        document.getElementById('cfgKeepWeekly').value = cfg.retention.keep_weekly || 4;
        document.getElementById('cfgKeepMonthly').value = cfg.retention.keep_monthly || 12;
      }

      configuredBotTokens = cfg.bot_tokens || [];
      renderBotTable();

      // Check setup status banner
      const banner = document.getElementById('bannerSetupAlert');
      if (banner) {
        if (!cfg.channel_id || configuredBotTokens.length === 0) {
          banner.style.display = 'block';
        } else {
          banner.style.display = 'none';
        }
      }
    } catch (err) {
      console.error('Settings load error:', err);
    }
  }

  function renderBotTable() {
    const tbody = document.getElementById('botConfigTableBody');
    const badge = document.getElementById('badgeMeshCount');
    if (badge) badge.innerText = `${configuredBotTokens.length} Bots Active`;

    if (!configuredBotTokens || configuredBotTokens.length === 0) {
      tbody.innerHTML = '<tr><td colspan="4" style="text-align: center; padding: 12px; color: var(--text-dim);">Chưa có bot nào trong Mesh. Vui lòng nhập token ở trên.</td></tr>';
      return;
    }

    tbody.innerHTML = configuredBotTokens.map((tok, idx) => {
      const prefix = tok.length > 10 ? tok.substring(0, 6) + '...' + tok.substring(tok.length - 4) : tok;
      return `
        <tr style="border-bottom: 1px solid var(--border-subtle);">
          <td style="padding: 6px 10px; font-family: var(--font-mono);">${prefix}</td>
          <td style="padding: 6px 10px;" id="botUser_${idx}">Bot #${idx + 1}</td>
          <td style="padding: 6px 10px;">
            <span class="chip chip-success" id="botStatus_${idx}">Sẵn sàng</span>
          </td>
          <td style="padding: 6px 10px; text-align: right;">
            <button type="button" class="btn btn-sm" style="font-size: 10px; padding: 2px 6px;" onclick="window.testSingleBot('${tok}', ${idx})">
              Test
            </button>
            <button type="button" class="btn btn-sm" style="font-size: 10px; padding: 2px 6px; color: var(--danger); margin-left: 4px;" onclick="window.removeBotToken(${idx})">
              Xóa
            </button>
          </td>
        </tr>
      `;
    }).join('');
  }

  window.removeBotToken = (idx) => {
    configuredBotTokens.splice(idx, 1);
    renderBotTable();
  };

  window.testSingleBot = async (tok, idx) => {
    const statusEl = document.getElementById(`botStatus_${idx}`);
    const userEl = document.getElementById(`botUser_${idx}`);
    if (statusEl) {
      statusEl.className = 'chip chip-warning';
      statusEl.innerText = 'Ping...';
    }

    try {
      const res = await fetch('/api/telegram/test', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ token: tok, channel_id: document.getElementById('cfgChannelID').value })
      });
      const data = await res.json();
      if (data.bots && data.bots[0] && data.bots[0].ok) {
        if (statusEl) {
          statusEl.className = 'chip chip-success';
          statusEl.innerText = '200 OK';
        }
        if (userEl && data.bots[0].username) {
          userEl.innerText = data.bots[0].username;
        }
      } else {
        if (statusEl) {
          statusEl.className = 'chip chip-danger';
          statusEl.innerText = 'Lỗi token';
        }
      }
    } catch (e) {
      if (statusEl) {
        statusEl.className = 'chip chip-danger';
        statusEl.innerText = 'Lỗi mạng';
      }
    }
  };

  // Add Bot Token
  const btnAddBot = document.getElementById('btnAddBotToken');
  if (btnAddBot) {
    btnAddBot.addEventListener('click', () => {
      const input = document.getElementById('inputNewBotToken');
      const val = input.value.trim();
      if (!val) {
        alert('Vui lòng nhập Telegram Bot Token!');
        return;
      }
      if (configuredBotTokens.includes(val)) {
        alert('Token này đã có trong danh sách!');
        return;
      }
      configuredBotTokens.push(val);
      renderBotTable();
      input.value = '';
    });
  }

  // Test All Bots in Mesh
  const btnTestAll = document.getElementById('btnTestAllBots');
  if (btnTestAll) {
    btnTestAll.addEventListener('click', async () => {
      const resultBox = document.getElementById('meshTestResult');
      if (resultBox) {
        resultBox.style.display = 'block';
        resultBox.style.color = 'var(--text-dim)';
        resultBox.innerText = 'Đang kiểm tra kết nối tới Telegram API cho tất cả bot...';
      }

      try {
        const res = await fetch('/api/telegram/test', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            tokens: configuredBotTokens,
            channel_id: document.getElementById('cfgChannelID').value
          })
        });
        const data = await res.json();
        if (data.ok) {
          if (resultBox) {
            resultBox.style.color = '#3fb950';
            resultBox.innerText = `Kiểm tra hoàn tất: Toàn bộ ${data.count} bot phản hồi 200 OK! Kênh ID: ${data.channel_id || 'Chưa điền'}`;
          }
        } else {
          if (resultBox) {
            resultBox.style.color = '#f85149';
            resultBox.innerText = `Cảnh báo: Có lỗi kết nối token hoặc chưa có bot nào hợp lệ.`;
          }
        }
      } catch (err) {
        if (resultBox) {
          resultBox.style.color = '#f85149';
          resultBox.innerText = 'Lỗi kết nối kiểm tra: ' + err.message;
        }
      }
    });
  }

  // Password toggle
  const btnTogglePass = document.getElementById('btnTogglePassphrase');
  if (btnTogglePass) {
    btnTogglePass.addEventListener('click', () => {
      const input = document.getElementById('cfgEncryptionKey');
      if (input.type === 'password') {
        input.type = 'text';
        btnTogglePass.innerText = 'Ẩn mật khẩu';
      } else {
        input.type = 'password';
        btnTogglePass.innerText = 'Hiện mật khẩu';
      }
    });
  }

  // Source path preset chips
  document.querySelectorAll('.btn-preset-source').forEach(btn => {
    btn.addEventListener('click', () => {
      const p = btn.getAttribute('data-path');
      if (p) document.getElementById('cfgSourcePath').value = p;
    });
  });

  // Save All Config
  const btnSaveAll = document.getElementById('btnSaveAllConfig');
  if (btnSaveAll) {
    btnSaveAll.addEventListener('click', async () => {
      const rawExcludes = document.getElementById('cfgExcludes').value;
      const excludes = rawExcludes.split(',').map(s => s.trim()).filter(Boolean);

      const payload = {
        channel_id: document.getElementById('cfgChannelID').value.trim(),
        bot_tokens: configuredBotTokens,
        encryption_key: document.getElementById('cfgEncryptionKey').value,
        node_name: document.getElementById('cfgNodeName').value.trim(),
        node_mode: document.getElementById('cfgNodeMode').value,
        source_path: document.getElementById('cfgSourcePath').value.trim(),
        chunk_size: parseInt(document.getElementById('cfgChunkSize').value, 10) || 51380224,
        bandwidth_schedule: document.getElementById('cfgBwSchedule').value.trim(),
        excludes: excludes,
        retention: {
          keep_hourly: parseInt(document.getElementById('cfgKeepHourly').value, 10) || 24,
          keep_daily: parseInt(document.getElementById('cfgKeepDaily').value, 10) || 7,
          keep_weekly: parseInt(document.getElementById('cfgKeepWeekly').value, 10) || 4,
          keep_monthly: parseInt(document.getElementById('cfgKeepMonthly').value, 10) || 12
        }
      };

      try {
        const res = await fetch('/api/config', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload)
        });
        if (!res.ok) {
          alert('Lỗi lưu cấu hình: ' + (await res.text()));
          return;
        }
        alert('Cấu hình Telegram Mesh và QoS đã được lưu và kích hoạt thành công!');
        loadSettings();
        refreshStatus();
        refreshBots();
      } catch (err) {
        alert('Lỗi lưu cấu hình: ' + err.message);
      }
    });
  }

  // Quick Go To Settings button from Banner
  const btnGoToSettings = document.getElementById('btnGoToSettings');
  if (btnGoToSettings) {
    btnGoToSettings.addEventListener('click', () => {
      document.querySelectorAll('.nav-tab').forEach(t => t.classList.remove('active'));
      document.querySelectorAll('.content-section').forEach(s => s.classList.remove('active'));
      const settingsTab = document.querySelector('.nav-tab[data-tab="settings"]');
      if (settingsTab) settingsTab.classList.add('active');
      const settingsSection = document.getElementById('section-settings');
      if (settingsSection) settingsSection.classList.add('active');
      loadSettings();
    });
  }

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
  loadSettings();
  initSSE();
  setInterval(refreshStatus, 5000);
});
