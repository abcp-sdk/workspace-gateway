'use strict';
(() => {
  const $ = (id) => document.getElementById(id)
  const WS = '/worker.v1.WorkerService/'
  const EN = '/worker.v1.WorkerEnroll/'
  const TOKEN_KEY = 'agent-worker.token'
  const CWD_KEY = 'agent-worker.cwd'

  let token = localStorage.getItem(TOKEN_KEY) || ''
  let workspace = '/workspace'
  let cwd = localStorage.getItem(CWD_KEY) || '' // workspace-relative ('' = root)
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

  function enterApp() {
    $('gate').classList.add('hidden')
    $('app').classList.remove('hidden')
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
      workspace = i.workspace || '/workspace'
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
      workspace = i.workspace || '/workspace'
      pill.textContent = `${i.os}/${i.arch} · ${i.workspace}`
      pill.className = 'pill ok'
      pill.title = 'boot_id ' + i.bootId + (i.droppedLines ? ` · dropped ${i.droppedLines}` : '')
    } catch (e) {
      if (e.code === 'unauthenticated') { showGate('Session expired — sign in again.'); return }
      pill.textContent = 'offline'
      pill.className = 'pill err'
      pill.title = String(e)
    }
  }

  // ------------------------------------------------------------------ shell
  const term = $('term')

  function absCwd() {
    return cwd ? workspace.replace(/\/+$/, '') + '/' + cwd : workspace
  }
  function renderPrompt() {
    $('prompt-cwd').textContent = absCwd()
    localStorage.setItem(CWD_KEY, cwd)
  }
  function write(text, cls) {
    const span = document.createElement('span')
    if (cls) span.className = cls
    span.textContent = text
    term.appendChild(span)
  }
  function writeCmd(cmd) {
    write(absCwd() + ' $ ', 'cmd cwd')
    write(cmd + '\n', 'cmd')
  }
  function writeOutput(line, isErr) {
    // Each output line is one element so stderr can be tinted and long lines wrap.
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
    const raw = input.value
    const cmd = raw.trim()
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
    // Client-side shell builtins (the worker has no persistent session).
    if (cmd === 'clear') { term.textContent = ''; return }
    const cd = cmd.match(/^cd(?:\s+(.+))?$/)
    if (cd) {
      const target = (cd[1] || '').trim()
      applyCd(target)
      return
    }
    try {
      const r = await rpc('Execute', { command: cmd, workdir: cwd })
      $('stop').disabled = false
      watchJob(r.jobId)
    } catch (e) {
      writeOutput('error: ' + e.message, true)
      scrollBottom()
    }
  }

  async function applyCd(target) {
    // Resolve against the virtual cwd; verify with FileList (isDir).
    let next = cwd
    if (!target || target === '~' || target === '/') next = ''
    else if (target === '..') next = cwd.split('/').slice(0, -1).join('/')
    else if (target.startsWith('/')) next = target.replace(/^\/+/, '')
    else next = (cwd ? cwd + '/' : '') + target
    next = next.replace(/\/+/g, '/').replace(/^\/+|\/+$/g, '')
    try {
      const r = await rpc('FileList', { path: next || '.', depth: 1, limit: 1 })
      if (!r.isDir) { writeOutput('cd: not a directory: ' + (target || '/'), true); scrollBottom(); return }
      cwd = next
      renderPrompt()
    } catch (e) {
      writeOutput('cd: ' + e.message, true)
      scrollBottom()
    }
  }

  $('stop').addEventListener('click', async () => {
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

  // ------------------------------------------------------------------ files
  let expanded = new Set()

  $('toggle-files').addEventListener('click', () => { $('files').classList.toggle('hidden'); if (!$('files').classList.contains('hidden')) renderTree() })
  $('f-close').addEventListener('click', () => $('files').classList.add('hidden'))
  $('toggle-jobs').addEventListener('click', () => { $('jobs').classList.toggle('hidden'); if (!$('jobs').classList.contains('hidden')) loadJobs() })
  $('j-close').addEventListener('click', () => $('jobs').classList.add('hidden'))

  async function renderTree() {
    const box = $('f-tree')
    box.textContent = ''
    $('f-crumb').textContent = absCwd()
    await renderDir(cwd, box, 0)
  }

  async function renderDir(path, parent, depth) {
    try {
      const r = await rpc('FileList', { path: path || '.', depth: 1, limit: 1000 })
      if (!r.isDir) return
      for (const f of (r.files || [])) {
        const rel = f.path
        const node = document.createElement('div')
        node.className = 'node' + (f.isDir ? ' dir' : '')
        node.style.paddingLeft = 6 + depth * 14 + 'px'
        const isOpen = expanded.has(rel)
        node.innerHTML = `<span class="tw">${f.isDir ? (isOpen ? '▾' : '▸') : ''}</span>` +
          `<span class="truncate">${escapeHtml(basename(rel))}</span>` +
          `<span class="sz">${f.isDir ? '' : fmtSize(f.size)}</span>`
        node.onclick = async () => {
          if (f.isDir) {
            if (expanded.has(rel)) { expanded.delete(rel) } else { expanded.add(rel) }
            renderTree()
          } else {
            openFile(rel)
          }
        }
        parent.appendChild(node)
        if (f.isDir && isOpen) await renderDir(rel, parent, depth + 1)
      }
    } catch (e) { toast(e.message, true) }
  }

  $('f-new').addEventListener('click', () => {
    const name = prompt('new file path (relative to workspace)')
    if (!name) return
    openFile(name.replace(/^\/+/, ''), '')
  })

  let editing = ''
  function openFile(path, initial) {
    editing = path
    $('f-name').textContent = '/' + path
    $('f-tree').classList.add('hidden')
    $('f-editor').classList.remove('hidden')
    if (initial !== undefined) { $('f-content').value = initial; return }
    rpc('FileRead', { path })
      .then((r) => { $('f-content').value = b64dec(r.content) })
      .catch((e) => toast(e.message, true))
  }
  $('f-back').addEventListener('click', () => { $('f-editor').classList.add('hidden'); $('f-tree').classList.remove('hidden'); renderTree() })
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
    if (!confirm('Delete /' + editing + '?')) return
    try { await rpc('FileDelete', { path: editing }); toast('deleted'); $('f-back').click() } catch (e) { toast(e.message, true) }
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
          $('jobs').classList.add('hidden')
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
        workspace = i.workspace || '/workspace'
        enterApp()
        return
      } catch { token = ''; localStorage.removeItem(TOKEN_KEY) }
    }
    showGate()
  })()
})()
