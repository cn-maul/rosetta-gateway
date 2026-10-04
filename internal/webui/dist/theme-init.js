// 暗色主题防闪（必须在 CSS 解析前同步执行，故外置为独立脚本文件）。
//
// 为什么不能内联在 index.html 的 <head> 里：管理面的 CSP 是
// script-src 'self'，**不含** 'unsafe-inline'。内联脚本会被浏览器静默拦掉 ——
// 主题防闪功能直接坏掉（暗色用户首帧白闪回归），而浏览器只在控制台报一条
// CSP 违规，构建与测试全绿，看不出任何异常。改成外置文件后同为 'self'，
// CSP 放行。
//
// 为什么不用 <script type="module">：module 是 defer 语义，要等文档解析完
// 才执行，那时 CSS 已经应用、首帧已经画完，防闪就失效了。必须是传统的
// 同步脚本 + <head> 位置。
//
// 逻辑与 src/App.vue 的 THEME_KEY / 主题三态回退保持一致；改动时两处要同步。
(function () {
  try {
    var saved = localStorage.getItem('rosetta_gw_theme');
    // saved === 'system'（或未设置）时跟随系统，'light' 时强制浅色。
    var dark = saved === 'dark' ||
      (saved !== 'light' && window.matchMedia('(prefers-color-scheme: dark)').matches);
    if (dark) {
      document.documentElement.setAttribute('data-theme', 'dark');
      var m = document.querySelector('meta[name=color-scheme]');
      if (m) m.setAttribute('content', 'dark');
    }
  } catch (e) {
    /* localStorage 不可用（隐私模式）时静默降级到浅色 */
  }
})();
