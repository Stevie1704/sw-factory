// Sends session commands to the coordinator through the UI's command inbox.
// The layout loads this script and writes the per-process command token in
// <meta name="factory-csrf-token"> when the session command API is on.
//
// window.factoryCommands.submit(command) posts one command and resolves with
// the final answer. A command is {session_id, kind, expected_revision,
// payload} and may name request_id; otherwise a new one is chosen. A lost
// answer is retried with the same request ID, so the coordinator applies the
// command at most once. An accepted command is not complete: submit polls
// the command until the coordinator records its outcome.
//
// A <form data-session-command="KIND" data-session-id="ID"
// data-expected-revision="N"> submits its named fields as the payload.
// A field with data-type="number" sends a number, a field with
// data-type="boolean" sends true for the value "true", a checkbox sends a
// boolean, and checkboxes with data-list send the list of their checked
// values. The form is marked data-dirty while it has unsent input, which
// pauses the automatic page refresh, so a refresh never wipes typed input.
// The outcome is written into the form's [data-command-status] element, and
// a "factory:command" event with the answer is sent from the form.
(function () {
  "use strict";

  var meta = document.querySelector('meta[name="factory-csrf-token"]');
  if (!meta) {
    return;
  }
  var token = meta.getAttribute("content");
  var pollMilliseconds = 1000;
  var maxSendAttempts = 3;

  // newRequestID returns a random request ID the store accepts.
  function newRequestID() {
    if (window.crypto && window.crypto.randomUUID) {
      return window.crypto.randomUUID();
    }
    var bytes = new Uint8Array(16);
    window.crypto.getRandomValues(bytes);
    return Array.prototype.map.call(bytes, function (b) {
      return ("0" + b.toString(16)).slice(-2);
    }).join("");
  }

  // readAnswer returns the JSON answer with its HTTP status.
  function readAnswer(response) {
    return response.json().then(function (answer) {
      answer.status = response.status;
      return answer;
    });
  }

  // send posts the command once. A network failure is retried with the same
  // request ID; an HTTP answer is final.
  function send(command, attempt) {
    return fetch("/api/commands", {
      method: "POST",
      credentials: "same-origin",
      cache: "no-store",
      headers: { "Content-Type": "application/json", "X-Factory-CSRF": token },
      body: JSON.stringify(command)
    }).then(readAnswer, function (error) {
      if (attempt >= maxSendAttempts) {
        throw error;
      }
      return send(command, attempt + 1);
    });
  }

  // wait polls an accepted command until it completes.
  function wait(answer) {
    if (answer.state !== "accepted" && answer.state !== "applying") {
      return Promise.resolve(answer);
    }
    return new Promise(function (resolve) {
      window.setTimeout(resolve, pollMilliseconds);
    }).then(function () {
      return fetch("/api/commands/" + encodeURIComponent(answer.request_id), {
        credentials: "same-origin",
        cache: "no-store"
      }).then(readAnswer, function () {
        return answer;
      });
    }).then(wait);
  }

  // submit posts command and resolves with its final answer.
  function submit(command) {
    command = Object.assign({ payload: {} }, command);
    if (!command.request_id) {
      command.request_id = newRequestID();
    }
    return send(command, 1).then(wait);
  }

  // describe returns a short readable text for one answer.
  function describe(answer) {
    var text = answer.outcome || answer.state || "unknown";
    return answer.detail ? text + ": " + answer.detail : text;
  }

  // payloadOf reads the named fields of form.
  function payloadOf(form) {
    var payload = {};
    Array.prototype.forEach.call(form.elements, function (field) {
      if (!field.name || field.disabled) {
        return;
      }
      if (field.type === "checkbox" && "list" in field.dataset) {
        payload[field.name] = payload[field.name] || [];
        if (field.checked) {
          payload[field.name].push(field.value);
        }
      } else if (field.type === "checkbox") {
        payload[field.name] = field.checked;
      } else if (field.dataset.type === "number") {
        payload[field.name] = Number(field.value);
      } else if (field.dataset.type === "boolean") {
        payload[field.name] = field.value === "true";
      } else {
        payload[field.name] = field.value;
      }
    });
    return payload;
  }

  // wire connects one command form.
  function wire(form) {
    if (form.dataset.commandWired) {
      return;
    }
    form.dataset.commandWired = "true";
    form.addEventListener("input", function () {
      form.dataset.dirty = "true";
    });
    form.addEventListener("submit", function (event) {
      event.preventDefault();
      var status = form.querySelector("[data-command-status]");
      if (status) {
        status.textContent = "sending";
      }
      // The form keeps its request ID until an answer arrives, so a submit
      // after a lost answer repeats the same request.
      if (!form.dataset.requestId) {
        form.dataset.requestId = newRequestID();
      }
      send({
        request_id: form.dataset.requestId,
        session_id: form.dataset.sessionId,
        kind: form.dataset.sessionCommand,
        expected_revision: Number(form.dataset.expectedRevision),
        payload: payloadOf(form)
      }, 1).then(function (answer) {
        if (status && (answer.state === "accepted" || answer.state === "applying")) {
          status.textContent = "accepted; waits for the coordinator";
        }
        return wait(answer);
      }).then(function (answer) {
        if (status) {
          status.textContent = describe(answer);
        }
        delete form.dataset.dirty;
        delete form.dataset.requestId;
        form.dispatchEvent(new CustomEvent("factory:command", { bubbles: true, detail: answer }));
      }, function () {
        if (status) {
          status.textContent = "UI server unreachable: submit again to retry the same request";
        }
      });
    });
  }

  // wireAll connects every command form on the page, also after a refresh
  // swapped the content.
  function wireAll() {
    Array.prototype.forEach.call(document.querySelectorAll("form[data-session-command]"), wire);
  }

  window.factoryCommands = { submit: submit, newRequestID: newRequestID };
  wireAll();
  new MutationObserver(wireAll).observe(document.body, { childList: true, subtree: true });
})();
