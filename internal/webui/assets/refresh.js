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

  // readPage returns the parsed page of an HTML response. Any status counts,
  // because a 503 error page is valid state. It throws for an answer that is
  // not HTML.
  function readPage(response) {
    var type = response.headers.get("Content-Type") || "";
    if (type.indexOf("text/html") !== 0) {
      throw new Error("refreshed answer is not HTML: " + type);
    }
    return response.text().then(function (html) {
      return new DOMParser().parseFromString(html, "text/html");
    });
  }

  // swapRegions replaces every region with its fresh copy. It throws before it
  // changes anything when the fresh page lacks a region, so a page that is
  // not a UI page counts as a failure.
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
      .then(readPage)
      .then(function (fresh) {
        swapRegions(fresh);
        showUnreachable(false);
      })
      .catch(function () {
        showUnreachable(true);
      });
  }

  window.setInterval(refresh, seconds * 1000);
  document.addEventListener("visibilitychange", refresh);
})();
