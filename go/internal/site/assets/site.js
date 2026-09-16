document.querySelectorAll('.tab:not(.absent)').forEach(function(b){
  b.onclick=function(){
    document.querySelectorAll('.tab').forEach(function(x){x.classList.remove('on')});
    document.querySelectorAll('.pane').forEach(function(x){x.classList.remove('on')});
    b.classList.add('on');
    document.getElementById('p-'+b.dataset.t).classList.add('on');
    history.replaceState(null,'','#'+b.dataset.t);
    window.scrollTo(0,0);
  };
});
(function(){
  var h=location.hash.slice(1);
  if(h){var t=document.querySelector('.tab[data-t="'+h+'"]:not(.absent)'); if(t)t.click();}
})();
var sel=document.getElementById('daysel');
if(sel) sel.onchange=function(){
  if(!this.value) return;                          // 停在占位项时不跳
  if(this.value==='@archive'){location.href='archive.html';return;}
  location.href=this.value+'.html'+location.hash;
};

function go(dir){
  var el=document.getElementById(dir>0?'nextday':'prevday');
  if(el&&el.href) location.href=el.href;
}
// 桌面：左右方向键换天
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
  function paint(){if(tg) tg.textContent = now()==='light' ? '☾' : '☀'}
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
// 表格还能继续横向滚时才显示右缘渐变
(function(){
  function sync(w){var d=w.firstElementChild;
    w.classList.toggle('more', d.scrollWidth-d.clientWidth-d.scrollLeft>4);}
  document.querySelectorAll('.tw').forEach(function(w){
    sync(w); w.firstElementChild.addEventListener('scroll',function(){sync(w)},{passive:true});
  });
  addEventListener('resize',function(){document.querySelectorAll('.tw').forEach(sync)});
})();
