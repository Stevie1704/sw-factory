// Refreshes the page content in place while the tab is visible.
// The layout loads this script only when automatic refresh is on and writes
// the interval in seconds as data-refresh-seconds on the body element.
(function () {
  "use strict";

  var seconds = Number(document.body.dataset.refreshSeconds);
  if (!(seconds > 0)) {
    return;
  }
  // Each refresh replaces these elements with the same ones from a fresh read.
  var regions = ["supervisor", "content"];
  var status = document.getElementById("refresh-status");

  // showUnreachable shows or hides the "UI server unreachable" state.
  function showUnreachable(unreachable) {
    if (status) {
      status.hidden = !unreachable;
    }
  }

  // swapRegions replaces every region with its fresh copy. It throws when the
  // fresh page lacks a region, so a non-page answer counts as a failure.
  function swapRegions(fresh) {
    var replacements = regions.map(function (id) {
      var next = fresh.getElementById(id);
      if (!next) {
        throw new Error("refreshed page has no #" + id);
      }
      return [document.getElementById(id), document.importNode(next, true)];
    });
    replacements.forEach(function (pair) {
      if (pair[0]) {
        pair[0].replaceWith(pair[1]);
      }
    });
  }

  // refresh reads the current page again and swaps its regions in place, so
  // the scroll position stays. A hidden tab skips the read.
  function refresh() {
    if (document.hidden) {
      return;
    }
    fetch(window.location.href, { cache: "no-store", credentials: "same-origin" })
      .then(function (response) {
        return response.text();
      })
      .then(function (html) {
        swapRegions(new DOMParser().parseFromString(html, "text/html"));
        showUnreachable(false);
      })
      .catch(function () {
        showUnreachable(true);
      });
  }

  window.setInterval(refresh, seconds * 1000);
  document.addEventListener("visibilitychange", refresh);
})();
