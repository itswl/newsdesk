// 栏目 tab：选中态同时写在 class 和 aria-selected 上，读屏靠后者知道选了哪一档。
// 缺报告的档也能点，面板里说明为什么没有、几点会有。
// 方向键有意不接管（留给换天，见下面 onkeydown），tab 之间用 Tab 键移动。
document.querySelectorAll('.tab').forEach(function(b){
  b.onclick=function(){
    document.querySelectorAll('.tab').forEach(function(x){
      x.classList.remove('on'); x.setAttribute('aria-selected','false');
    });
    document.querySelectorAll('.pane').forEach(function(x){x.classList.remove('on')});
    b.classList.add('on'); b.setAttribute('aria-selected','true');
    document.getElementById('p-'+b.dataset.t).classList.add('on');
    history.replaceState(null,'','#'+b.dataset.t);
    window.scrollTo(0,0);
  };
});
(function(){
  var h=location.hash.slice(1);
  if(h){var t=document.querySelector('.tab[data-t="'+h+'"]'); if(t)t.click();}
})();
// 换天/换档的导航共用这套反馈：先把正文压暗、等这一帧真的画出来再跳。
// 到 Cloudflare 边缘的线路在晚高峰可能几秒才回包，而 PWA 没有地址栏进度条，
// 不给反馈的话「点了跳转」和「死了」看起来一模一样（实测就这么被当成卡死）。
// 推迟一帧还能避开 iOS 上选择器面板还在收起时就发起导航的偶发冻结。
// 导航被取消（按了停止 / 回退）时由 5 秒兜底和 pageshow 把状态清掉——
// 返回键走 bfcache 的话页面原样复活，不能带着压暗的正文。
function leave(){
  var d=document.documentElement; d.classList.add('leaving');
  setTimeout(function(){d.classList.remove('leaving')},5000);
  requestAnimationFrame(function(){requestAnimationFrame(function(){d.classList.remove('leaving-wait')})});
}
function navTo(href){
  leave();
  requestAnimationFrame(function(){requestAnimationFrame(function(){location.href=href})});
}
var sel=document.getElementById('daysel');
if(sel) sel.onchange=function(){
  var v=this.value;
  if(!v) return;                                   // 停在占位项时不跳
  navTo((v==='@archive'?'archive.html':v+'.html')+location.hash);
};
addEventListener('pageshow',function(){
  document.documentElement.classList.remove('leaving');
});

function go(dir){
  var el=document.getElementById(dir>0?'nextday':'prevday');
  if(el&&el.href) navTo(el.href);
}
// 桌面：左右方向键换天。焦点落在 tab 上时也一样——APG 的 tabs 模式要求方向键在 tab
// 之间移动，这里有意偏离：换天是最高频的动作，而点完 tab 焦点就在它上面，若在这时
// 吞掉方向键，「点档、再按方向键换天」这条最常见的路径就断了。
document.onkeydown=function(e){
  if(e.target.tagName==='SELECT'||e.metaKey||e.ctrlKey) return;
  if(e.key==='ArrowLeft') go(-1);
  if(e.key==='ArrowRight') go(1);
};
// 手机：横向滑动换天。起点在可横向滚动的表格里时不接管，否则会和看表冲突。
(function(){
  var x0=null,y0=null,lock=false;
  addEventListener('touchstart',function(e){
    var t=e.touches[0]; x0=t.clientX; y0=t.clientY;
    lock=!!e.target.closest('.tw');
  },{passive:true});
  addEventListener('touchend',function(e){
    if(x0===null||lock) return;
    var t=e.changedTouches[0], dx=t.clientX-x0, dy=t.clientY-y0;
    if(Math.abs(dx)>70 && Math.abs(dx)>Math.abs(dy)*2) go(dx<0?1:-1);
    x0=null;
  },{passive:true});
})();
// 深浅色切换：未手动选过就跟随系统，选过之后记住
(function(){
  var tg=document.getElementById('theme');
  function sysLight(){return matchMedia('(prefers-color-scheme:light)').matches}
  function now(){return document.documentElement.dataset.theme || (sysLight()?'light':'dark')}
  // 状态栏／地址栏的颜色。<head> 里两条 meta 是按系统偏好分流的，媒体查询认不出
  // 「手动选过主题」这件事——浅色页面配着深色状态栏，在手机上很扎眼。所以手动
  // 选过之后由 JS 改写这两条并摘掉 media。颜色从 CSS 变量取，不再抄一份色值。
  function color(){
    var c=getComputedStyle(document.documentElement).getPropertyValue('--bg').trim();
    if(!c) return;
    document.querySelectorAll('meta[name=theme-color]').forEach(function(m){
      m.removeAttribute('media'); m.setAttribute('content',c);
    });
  }
  // 状态栏那块盖板：用内联样式显式写一次，别只靠继承的 CSS 变量——实测 iOS 上
  // 滚动之后那一条的旧背景不跟着变量重绘，只有对元素本身写样式才会。
  function notch(){
    var n=document.querySelector('.notch');
    if(!n) return;
    var c=getComputedStyle(document.documentElement).getPropertyValue('--bg2').trim();
    if(c) n.style.background=c;
  }
  function paint(){
    notch();
    if(tg){
      var light = now()==='light';
      tg.textContent = light ? '☾' : '☀';
      // 按钮的可访问名是「深色模式」，按下 = 深色已生效，读屏报「深色模式，已按下」。
      // 服务端渲染时还不知道读者选过什么，由这里在首屏补上，切换时再跟着改。
      tg.setAttribute('aria-pressed', light ? 'false' : 'true');
    }
    if(document.documentElement.dataset.theme) color();  // 没手动选过就别插手
  }
  paint();
  if(tg) tg.onclick=function(){
    var next = now()==='light' ? 'dark' : 'light';
    document.documentElement.dataset.theme = next;
    try{localStorage.setItem('theme',next)}catch(e){}
    paint();
  };
  // 没手动选过时，系统切换要跟着变
  matchMedia('(prefers-color-scheme:light)').addEventListener('change',function(){
    if(!document.documentElement.dataset.theme) paint();
  });
})();
// Service Worker：给「看过的页面」留一条不过网络的路。晚高峰到边缘的线路
// 可能整段不通（实测），没它的话每次打开都得赌线路。注册放在 load 之后不占
// 首屏；只有安全上下文才有（https 与 localhost；isSecureContext 是标准判定，
// 别退回比对 protocol——那会把 localhost 也拦掉）。失败静默：有它更好，没它照常读。
// sw.js 只在站点根（一个 SW 管整个源站，含语言子目录），所以注册用绝对路径。
if ('serviceWorker' in navigator && window.isSecureContext) {
  addEventListener('load', function () {
    navigator.serviceWorker.register('/sw.js').catch(function () {});
  });
}
// 表格还能继续横向滚时才显示右缘渐变
(function(){
  function sync(w){var d=w.firstElementChild;
    w.classList.toggle('more', d.scrollWidth-d.clientWidth-d.scrollLeft>4);}
  document.querySelectorAll('.tw').forEach(function(w){
    sync(w); w.firstElementChild.addEventListener('scroll',function(){sync(w)},{passive:true});
  });
  addEventListener('resize',function(){document.querySelectorAll('.tw').forEach(sync)});
})();

// 通知开关。文案全部由 Go 那边按站点语言塞进 data-*，这里一个中文字都不写死。
(function () {
  var btn = document.getElementById('pushtoggle');
  if (!btn) return;                                   // 没配公钥就没有这个按钮
  var note = document.getElementById('pushnote');
  var d = btn.dataset;
  var standalone = matchMedia('(display-mode: standalone)').matches || navigator.standalone === true;
  var can = 'serviceWorker' in navigator && 'PushManager' in window &&
            'Notification' in window && window.isSecureContext;

  if (!can) {
    // iOS 只给「添加到主屏幕」之后的 webapp 发推送，Safari 里连 PushManager 都没有。
    // 这时给一句话说清楚，别让人对着一个点了没反应的按钮。
    if (!standalone && /iPad|iPhone|iPod/.test(navigator.userAgent)) {
      btn.hidden = false; btn.disabled = true; btn.textContent = d.install;
    }
    return;
  }
  btn.hidden = false;

  var on = false;
  function say(t) { if (note) note.textContent = t ? ' ' + t : ''; }
  function paint(sub) { on = !!sub; btn.textContent = on ? d.off : d.on; btn.disabled = false; }
  function post(path, body) {
    return fetch(path, { method: 'POST', headers: { 'content-type': 'application/json' },
                         body: JSON.stringify(body) })
      .then(function (r) { return r.ok ? r.json() : null; });
  }
  function keyBytes(s) {
    var t = (s + '==='.slice((s.length + 3) % 4)).replace(/-/g, '+').replace(/_/g, '/');
    var raw = atob(t), out = new Uint8Array(raw.length);
    for (var i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
    return out;
  }

  navigator.serviceWorker.ready
    .then(function (reg) { return reg.pushManager.getSubscription(); })
    .then(paint).catch(function () { paint(null); });

  btn.onclick = function () {
    // ⚠️ 权限请求必须直接坐在点击里。套进 then 之后 Safari 会认为不是用户手势
    // 而直接拒绝——表现是点了没反应，控制台也不一定有话说。
    var ask = (on || Notification.permission === 'granted')
      ? Promise.resolve('granted') : Notification.requestPermission();

    btn.disabled = true; btn.textContent = d.wait; say('');
    var wasOn = on;
    ask.then(function (perm) {
      if (!wasOn && perm !== 'granted') { paint(null); say(perm === 'denied' ? d.denied : ''); return; }
      return navigator.serviceWorker.ready.then(function (reg) {
        return reg.pushManager.getSubscription().then(function (sub) {
          if (sub) {
            var ep = sub.endpoint;
            // 先退订再告诉服务端：反过来的话本地退订失败会留下一个收不到的订阅
            return sub.unsubscribe()
              .then(function () { return post('/push/unsubscribe', { endpoint: ep }); })
              .then(function () { paint(null); say(''); });
          }
          return reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: keyBytes(d.key) })
            .then(function (s) {
              var j = s.toJSON();
              return post('/push/subscribe',
                { endpoint: s.endpoint, p256dh: j.keys.p256dh, auth: j.keys.auth })
                .catch(function () { return null; })
                .then(function (r) {
                  // 服务端订阅成功会立刻回推一条，welcome 就是那一条的投递结果。失败时
                  // detail 是推送服务给的原因（如 403 BadJwtToken），原样带上：它不进任何
                  // 日志，这是唯一能看到它的地方
                  if (r && r.welcome !== 'gone') {
                    paint(true);
                    say(r.welcome === 'ok' ? d.ok : d.fail + (r.detail ? ' (' + r.detail + ')' : ''));
                    return;
                  }
                  // 服务端没收下（没配好、当天额度满了、断网），或者推送服务说这个订阅已失效
                  // （gone，服务端已经删了它）：本地这份也撤掉。留着的话按钮显示「已开启」
                  // 而服务端并没有它，再点一次反倒走进了上面的退订分支
                  return s.unsubscribe().catch(function () {})
                    .then(function () { paint(null); say(d.fail); });
                });
            });
        });
      });
    }).catch(function () { paint(null); say(d.fail); });
  };
})();
