'use strict';
(() => {
  const $ = (id) => document.getElementById(id)
  const WS = '/worker.v1.WorkerService/'
  const EN = '/worker.v1.WorkerEnroll/'
  const TOKEN_KEY = 'agent-worker.token'
  const CWD_KEY = 'agent-worker.cwd'

  let token = localStorage.getItem(TOKEN_KEY) || ''
  let workspace = '' // absolute workspace root (from Info); '' until known
  let cwd = localStorage.getItem(CWD_KEY) || '' // ABSOLUTE working directory
  let watchAbort = null

  // ------------------------------------------------------------------ utils
  const toast = (msg, err) => {
    const d = document.createElement('div')
    if (err) d.className = 'err'
    d.textContent = msg
    $('toast').appendChild(d)
    setTimeout(() => d.remove(), err ? 6000 : 3000)
  }
  const b64enc = (s) => btoa(unescape(encodeURIComponent(s)))
  const b64dec = (s) => decodeURIComponent(escape(atob(s || '')))
  const escapeHtml = (s) => String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]))
  const basename = (p) => String(p).replace(/\/+$/, '').split('/').pop() || p
  const fmtSize = (n) => {
    n = Number(n) || 0
    if (n < 1024) return n + ' B'
    if (n < 1048576) return (n / 1024).toFixed(1) + ' KiB'
    return (n / 1048576).toFixed(1) + ' MiB'
  }
  // Strip ANSI/VT escape sequences (CSI, OSC incl. hyperlinks, DCS, single-char).
  const stripAnsi = (s) =>
    String(s)
      .replace(/\u001b\][^\u0007\u001b]*(?:\u0007|\u001b\\)/g, '') // OSC … BEL/ST
      .replace(/\u001bP[\s\S]*?\u001b\\/g, '') // DCS
      .replace(/\u001b[@-Z\\-_]/g, '') // single-char escapes
      .replace(/\u001b\[[0-?]*[ -/]*[@-~]/g, '') // CSI

  // Normalize an ABSOLUTE path (collapse //, ., ..). The filesystem is
  // unconfined, so `..` at "/" stays "/".
  const normAbs = (p) => {
    const parts = []
    for (const seg of String(p).split('/')) {
      if (seg === '' || seg === '.') continue
      if (seg === '..') { if (parts.length) parts.pop(); continue }
      parts.push(seg)
    }
    return '/' + parts.join('/')
  }
  // Resolve a `cd`/nav target against a base ABSOLUTE dir.
  const resolveAbs = (base, target) => {
    const t = String(target == null ? '' : target).trim()
    if (t === '' || t === '~') return workspace || '/'
    if (t.startsWith('/')) return normAbs(t)
    return normAbs((base || '/') + '/' + t)
  }
  // A FileList entry's path is workspace-relative when inside the root and
  // absolute when outside — make it always absolute for navigation.
  const absOf = (p) => (String(p).startsWith('/') ? normAbs(p) : normAbs((workspace || '') + '/' + p))

  // ------------------------------------------------------------------ rpc
  async function rpc(method, body, base) {
    const res = await fetch((base || WS) + method, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'Connect-Protocol-Version': '1',
        ...(token ? { Authorization: 'Bearer ' + token } : {}),
      },
      body: JSON.stringify(body || {}),
    })
    const text = await res.text()
    let data = {}
    try { data = text ? JSON.parse(text) : {} } catch { data = { message: text } }
    if (!res.ok) {
      const e = new Error(data.message || data.code || ('HTTP ' + res.status))
      e.code = data.code || String(res.status)
      throw e
    }
    return data
  }

  // ------------------------------------------------------------- login gate
  async function showGate(errMsg) {
    $('app').classList.add('hidden')
    $('gate').classList.remove('hidden')
    if (errMsg) $('gate-err').textContent = errMsg
    try {
      const s = await rpc('Status', {}, EN)
      $('gate-claim-box').classList.toggle('hidden', !s.needsCode)
    } catch { /* enroll optional */ }
  }

  async function enterApp() {
    $('gate').classList.add('hidden')
    $('app').classList.remove('hidden')
    // Validate a stored cwd (it may be stale after a workspace change); fall
    // back to the workspace root when it no longer resolves to a directory.
    if (cwd) {
      try {
        const r = await rpc('FileList', { path: cwd, depth: 1, limit: 1 })
        if (!r.isDir) cwd = workspace || '/'
      } catch { cwd = workspace || '/' }
    }
    if (!cwd) cwd = workspace || '/'
    treeRoot = cwd
    refreshInfo()
    renderPrompt()
    $('prompt-input').focus()
  }

  async function connect(tok) {
    tok = (tok || '').trim()
    if (!tok) { $('gate-err').textContent = 'Enter a token.'; return }
    $('gate-connect').disabled = true
    $('gate-err').textContent = ''
    token = tok
    try {
      const i = await rpc('Info', {})
      workspace = i.workspace || '/'
      if (!cwd) cwd = workspace
      localStorage.setItem(TOKEN_KEY, tok)
      enterApp()
    } catch (e) {
      token = ''
      localStorage.removeItem(TOKEN_KEY)
      $('gate-err').textContent =
        e.code === 'unauthenticated' ? 'Invalid token — access denied.'
          : 'Cannot reach the worker: ' + e.message
    } finally {
      $('gate-connect').disabled = false
    }
  }

  $('gate-form').addEventListener('submit', (ev) => { ev.preventDefault(); connect($('gate-token').value) })
  $('gate-claim').addEventListener('click', async () => {
    const code = $('gate-code').value.trim()
    if (!code) return
    try {
      const r = await rpc('Claim', { code, ownerId: 'webui' }, EN)
      $('gate-token').value = r.token
      await connect(r.token)
    } catch (e) {
      $('gate-err').textContent = 'Claim failed: ' + e.message
    }
  })
  $('signout').addEventListener('click', () => {
    token = ''
    localStorage.removeItem(TOKEN_KEY)
    $('gate-token').value = ''
    $('gate-err').textContent = ''
    showGate()
  })

  async function refreshInfo() {
    const pill = $('info-pill')
    try {
      const i = await rpc('Info', {})
      workspace = i.workspace || workspace
      pill.textContent = `${i.os}/${i.arch}`
      pill.title = 'workspace ' + i.workspace + ' · boot_id ' + i.bootId + (i.droppedLines ? ` · dropped ${i.droppedLines}` : '')
      pill.className = 'pill ok'
    } catch (e) {
      if (e.code === 'unauthenticated') { showGate('Session expired — sign in again.'); return }
      pill.textContent = 'offline'
      pill.className = 'pill err'
      pill.title = String(e)
    }
  }

  // ------------------------------------------------------------------ shell
  const term = $('term')

  function renderPrompt() {
    $('prompt-cwd').textContent = cwd || workspace || '/'
    localStorage.setItem(CWD_KEY, cwd)
  }
  function write(text, cls) {
    const span = document.createElement('span')
    if (cls) span.className = cls
    span.textContent = text
    term.appendChild(span)
  }
  function writeCmd(cmd) {
    write((cwd || '/') + ' $ ', 'cmd cwd')
    write(cmd + '\n', 'cmd')
  }
  function writeOutput(line, isErr) {
    const div = document.createElement('div')
    if (isErr) div.className = 'err'
    div.textContent = stripAnsi(line)
    term.appendChild(div)
  }
  function scrollBottom() { term.scrollTop = term.scrollHeight }

  const history = []
  let histIdx = -1

  $('prompt-form').addEventListener('submit', (ev) => {
    ev.preventDefault()
    const input = $('prompt-input')
    const cmd = input.value.trim()
    input.value = ''
    if (!cmd) return
    history.push(cmd); histIdx = history.length
    runCommand(cmd)
  })
  $('prompt-input').addEventListener('keydown', (ev) => {
    if (ev.key === 'ArrowUp') { ev.preventDefault(); if (histIdx > 0) { histIdx--; $('prompt-input').value = history[histIdx] } }
    else if (ev.key === 'ArrowDown') { ev.preventDefault(); if (histIdx < history.length - 1) { histIdx++; $('prompt-input').value = history[histIdx] } else { histIdx = history.length; $('prompt-input').value = '' } }
    else if (ev.key === 'c' && ev.ctrlKey) { $('stop').click() }
  })

  async function runCommand(cmd) {
    writeCmd(cmd)
    scrollBottom()
    // Client-side builtins (the worker has no persistent shell session).
    if (cmd === 'clear') { term.textContent = ''; return }
    const cd = cmd.match(/^cd(?:\s+(.+))?$/)
    if (cd) { await applyCd(cd[1]); return }
    try {
      const r = await rpc('Execute', { command: cmd, workdir: cwd || workspace })
      $('stop').disabled = false
      watchJob(r.jobId)
    } catch (e) {
      writeOutput('error: ' + e.message, true)
      scrollBottom()
    }
  }

  // `cd` changes only the CLIENT-side prompt dir (the worker is stateless); the
  // dir is passed as each command's workdir. Paths are unconfined, so any
  // absolute dir the worker can stat is allowed.
  async function applyCd(target) {
    const next = resolveAbs(cwd || workspace || '/', target)
    try {
      const r = await rpc('FileList', { path: next, depth: 1, limit: 1 })
      if (!r.isDir) { writeOutput('cd: not a directory: ' + (target || '/'), true); scrollBottom(); return }
      cwd = next
      renderPrompt()
    } catch (e) {
      writeOutput('cd: ' + e.message, true)
      scrollBottom()
    }
  }

  $('stop').addEventListener('click', () => {
    if (watchAbort) { watchAbort.abort(); writeOutput('\n[stopped]', true) }
  })

  // WatchJob server-stream (Connect streaming envelope: 5-byte header frames).
  async function watchJob(id) {
    if (watchAbort) watchAbort.abort()
    const ac = new AbortController(); watchAbort = ac
    try {
      const res = await fetch(WS + 'WatchJob', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/connect+json',
          'Connect-Protocol-Version': '1',
          ...(token ? { Authorization: 'Bearer ' + token } : {}),
        },
        body: frame(JSON.stringify({ jobId: id })),
        signal: ac.signal,
      })
      if (!res.ok) throw new Error('watch HTTP ' + res.status + ' ' + (await res.text()).slice(0, 200))
      const reader = res.body.getReader()
      let buf = new Uint8Array(0)
      for (;;) {
        const { value, done } = await reader.read()
        if (done) break
        buf = concat(buf, value)
        while (buf.length >= 5) {
          const len = (buf[1] << 24) | (buf[2] << 16) | (buf[3] << 8) | buf[4]
          if (buf.length < 5 + len) break
          const flag = buf[0]
          const payload = new TextDecoder().decode(buf.subarray(5, 5 + len))
          buf = buf.subarray(5 + len)
          if (flag & 0x02) continue
          if (flag & 0x01) {
            let m = payload
            try { m = JSON.parse(payload).message || payload } catch { /* raw */ }
            writeOutput('[error] ' + m, true)
            continue
          }
          let msg
          try { msg = JSON.parse(payload) } catch { continue }
          if (msg.output !== undefined) { writeOutput(msg.output, false); scrollBottom() }
          if (msg.done) {
            const code = msg.done.exitCode ?? 0
            write('\n[done exit=' + code + ']\n', 'exit' + (code ? ' bad' : ''))
            scrollBottom()
            $('stop').disabled = true
            return
          }
        }
      }
    } catch (e) {
      if (e.name !== 'AbortError') { writeOutput('[watch failed] ' + e.message, true); scrollBottom() }
    } finally {
      if (watchAbort === ac) watchAbort = null
      $('stop').disabled = true
    }
  }

  function frame(str) {
    const bytes = new TextEncoder().encode(str)
    const out = new Uint8Array(5 + bytes.length)
    out[1] = (bytes.length >>> 24) & 255; out[2] = (bytes.length >>> 16) & 255
    out[3] = (bytes.length >>> 8) & 255; out[4] = bytes.length & 255
    out.set(bytes, 5)
    return out
  }
  function concat(a, b) { const o = new Uint8Array(a.length + b.length); o.set(a); o.set(b, a.length); return o }

  // ------------------------------------------------------- drawers (exclusive)
  function openDrawer(which) {
    const files = $('files'), jobs = $('jobs')
    if (which === 'files') {
      jobs.classList.add('hidden')
      files.classList.remove('hidden')
      // Follow the shell's current directory each time the drawer opens.
      treeRoot = cwd || workspace || '/'
      renderTree()
    } else if (which === 'jobs') {
      files.classList.add('hidden')
      jobs.classList.remove('hidden')
      loadJobs()
    } else {
      files.classList.add('hidden')
      jobs.classList.add('hidden')
    }
    syncDrawerButtons()
  }
  function syncDrawerButtons() {
    $('toggle-files').classList.toggle('active', !$('files').classList.contains('hidden'))
    $('toggle-jobs').classList.toggle('active', !$('jobs').classList.contains('hidden'))
  }
  $('toggle-files').addEventListener('click', () => openDrawer($('files').classList.contains('hidden') ? 'files' : null))
  $('toggle-jobs').addEventListener('click', () => openDrawer($('jobs').classList.contains('hidden') ? 'jobs' : null))
  $('f-close').addEventListener('click', () => openDrawer(null))
  $('j-close').addEventListener('click', () => openDrawer(null))

  // ------------------------------------------------------------------ files
  let expanded = new Set()
  let treeRoot = null // absolute dir currently shown in the tree

  async function renderTree() {
    const box = $('f-tree')
    box.textContent = ''
    if (treeRoot === null) treeRoot = cwd || workspace || '/'
    $('f-crumb').textContent = treeRoot
    $('f-tree').classList.remove('hidden')
    $('f-editor').classList.add('hidden')
    await renderDir(treeRoot, box, 0)
  }

  async function renderDir(dir, parent, depth) {
    try {
      const r = await rpc('FileList', { path: dir, depth: 1, limit: 1000 })
      if (!r.isDir) return
      for (const f of (r.files || [])) {
        const abs = absOf(f.path)
        const node = document.createElement('div')
        node.className = 'node' + (f.isDir ? ' dir' : '')
        node.style.paddingLeft = 6 + depth * 14 + 'px'
        const isOpen = expanded.has(abs)
        node.innerHTML = `<span class="tw">${f.isDir ? (isOpen ? '▾' : '▸') : ''}</span>` +
          `<span class="truncate">${escapeHtml(basename(abs))}</span>` +
          `<span class="sz">${f.isDir ? '' : fmtSize(f.size)}</span>`
        node.onclick = async () => {
          if (f.isDir) {
            if (expanded.has(abs)) expanded.delete(abs); else expanded.add(abs)
            renderTree()
          } else {
            openFile(abs)
          }
        }
        parent.appendChild(node)
        if (f.isDir && isOpen) await renderDir(abs, parent, depth + 1)
      }
    } catch (e) { toast(e.message, true) }
  }

  $('f-up').addEventListener('click', () => { treeRoot = normAbs(treeRoot + '/..'); renderTree() })
  $('f-home').addEventListener('click', () => { treeRoot = workspace || '/'; renderTree() })
  $('f-new').addEventListener('click', () => {
    const name = prompt('new file path (absolute, or relative to ' + treeRoot + ')')
    if (!name) return
    openFile(resolveAbs(treeRoot, name), '')
  })

  let editing = ''
  function openFile(path, initial) {
    editing = path
    $('f-name').textContent = path
    $('f-tree').classList.add('hidden')
    $('f-editor').classList.remove('hidden')
    if (initial !== undefined) { $('f-content').value = initial; return }
    rpc('FileRead', { path })
      .then((r) => { $('f-content').value = b64dec(r.content) })
      .catch((e) => toast(e.message, true))
  }
  $('f-back').addEventListener('click', () => { renderTree() })
  $('f-save').addEventListener('click', async () => {
    try { await rpc('FileWrite', { path: editing, content: b64enc($('f-content').value) }); toast('saved ' + editing) } catch (e) { toast(e.message, true) }
  })
  $('f-dl').addEventListener('click', async () => {
    try {
      const r = await rpc('FileRead', { path: editing })
      const bytes = Uint8Array.from(atob(r.content || ''), (c) => c.charCodeAt(0))
      const url = URL.createObjectURL(new Blob([bytes]))
      const a = document.createElement('a'); a.href = url; a.download = basename(editing); a.click()
      URL.revokeObjectURL(url)
    } catch (e) { toast(e.message, true) }
  })
  $('f-del').addEventListener('click', async () => {
    if (!confirm('Delete ' + editing + '?')) return
    try { await rpc('FileDelete', { path: editing }); toast('deleted'); renderTree() } catch (e) { toast(e.message, true) }
  })

  // ------------------------------------------------------------------ jobs
  $('j-refresh').addEventListener('click', loadJobs)
  async function loadJobs() {
    try {
      const r = await rpc('ListJobs', { limit: 200 })
      const tb = $('j-list'); tb.textContent = ''
      for (const j of (r.jobs || [])) {
        const tr = document.createElement('tr')
        tr.innerHTML = `<td class="mono">${j.id}</td><td class="state-${j.state}">${j.state}</td>` +
          `<td>${j.state === 'running' ? '' : (j.exitCode ?? 0)}</td>` +
          `<td class="cmd" title="${escapeHtml(j.command)}">${escapeHtml(j.command)}</td>`
        tr.querySelector('.cmd').onclick = () => {
          openDrawer(null)
          writeCmd(j.command)
          watchJob(j.id)
        }
        tb.appendChild(tr)
      }
    } catch (e) { toast(e.message, true) }
  }

  // ------------------------------------------------------------------ boot
  ;(async () => {
    if (token) {
      try {
        const i = await rpc('Info', {})
        workspace = i.workspace || '/'
        if (!cwd) cwd = workspace
        enterApp()
        return
      } catch { token = ''; localStorage.removeItem(TOKEN_KEY) }
    }
    showGate()
  })()
})()
