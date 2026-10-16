// Behavior of the ideation session pages on top of commands.js: start a
// session from an idea, compose one answer from the per-question fields,
// edit the brief, send a form with Ctrl+Enter, and reload the page after a
// command is applied so the page shows the new session state.
//
// Every listener is on the document, so content that the automatic refresh
// swaps in keeps working without a second wiring pass.
(function () {
  "use strict";

  var commands = window.factoryCommands;
  if (!commands) {
    return;
  }

  // statusOf returns the status element of form.
  function statusOf(form) {
    return form.querySelector("[data-command-status]");
  }

  // setStatus writes text into the status element of form.
  function setStatus(form, text) {
    var status = statusOf(form);
    if (status) {
      status.textContent = text;
    }
  }

  // describe returns a short readable text for one answer or transport
  // failure.
  function describe(answer) {
    if (!commands.isAnswer(answer)) {
      return "UI server unreachable: send again to retry the same request";
    }
    var text = answer.outcome || answer.state || "unknown";
    return answer.detail ? text + ": " + answer.detail : text;
  }

  // requestID returns the request ID that form keeps for one step until an
  // answer arrives, so a resend after a lost answer repeats the request.
  function requestID(form, step) {
    var key = "request" + step;
    if (!form.dataset[key]) {
      form.dataset[key] = commands.newRequestID();
    }
    return form.dataset[key];
  }

  // finish clears the dirty mark of form and reloads the page when the
  // command was applied; otherwise it shows the outcome.
  function finish(form, answer) {
    setStatus(form, describe(answer));
    if (answer && answer.outcome === "applied") {
      delete form.dataset.dirty;
      window.location.reload();
    }
  }

  // composeAnswer writes the per-question answers and the note into the
  // hidden body field. An empty answer is left out, so its question stays
  // open.
  function composeAnswer(form) {
    var body = form.querySelector("[data-answer-body]");
    if (!body) {
      return;
    }
    var parts = [];
    Array.prototype.forEach.call(form.querySelectorAll("[data-question-id]"), function (field) {
      var text = field.value.trim();
      if (text) {
        parts.push("Answer to " + field.dataset.questionId + " (" + field.dataset.questionPrompt + "):\n" + text);
      }
    });
    var note = form.querySelector("[data-answer-note]");
    if (note && note.value.trim()) {
      parts.push(note.value.trim());
    }
    body.value = parts.join("\n\n");
  }

  // startSession creates a session and sends the idea as its first turn,
  // then opens the session page. A created session is not created again
  // when only the idea must be resent.
  function startSession(form) {
    var idea = form.elements.idea.value.trim();
    var title = form.elements.title.value.trim();
    if (!form.dataset.sessionId) {
      form.dataset.sessionId = commands.newRequestID();
    }
    var sessionID = form.dataset.sessionId;
    var created = form.dataset.createdRevision ? Promise.resolve({ outcome: "applied", session_revision: Number(form.dataset.createdRevision) }) : null;
    if (!created) {
      setStatus(form, "creating the session");
      created = commands.submit({ request_id: requestID(form, "Create"), session_id: sessionID, kind: "create_session", expected_revision: 0, payload: { title: title } });
    }
    created.then(function (answer) {
      if (answer.outcome !== "applied") {
        throw answer;
      }
      form.dataset.createdRevision = String(answer.session_revision);
      setStatus(form, "sending your idea");
      return commands.submit({ request_id: requestID(form, "Turn"), session_id: sessionID, kind: "submit_turn", expected_revision: answer.session_revision, payload: { body: idea } });
    }).then(function (answer) {
      if (answer.outcome !== "applied") {
        throw answer;
      }
      delete form.dataset.dirty;
      window.location.href = "/sessions/" + encodeURIComponent(sessionID);
    }).catch(function (answer) {
      setStatus(form, describe(answer));
      if (commands.isAnswer(answer)) {
        // The server answered, so a resend is a new request. A transport
        // failure keeps every ID, so a resend replays the same requests.
        delete form.dataset.requestTurn;
        if (!form.dataset.createdRevision) {
          delete form.dataset.requestCreate;
          delete form.dataset.sessionId;
        }
      }
    });
  }

  // rowsOf returns the brief rows inside container.
  function rowsOf(container) {
    return Array.prototype.slice.call(container.querySelectorAll("[data-brief-row]"));
  }

  // briefOf reads the complete brief from the editor. Rows without text are
  // left out.
  function briefOf(form) {
    var brief = {
      problem: form.querySelector('[data-brief-field="problem"]').value.trim(),
      audience: form.querySelector('[data-brief-field="audience"]').value.trim(),
      decisions: []
    };
    rowsOf(form.querySelector("[data-brief-decisions]")).forEach(function (row) {
      var decision = row.querySelector("[data-row-decision]").value.trim();
      if (decision) {
        brief.decisions.push({ id: row.dataset.itemId, decision: decision, rationale: row.querySelector("[data-row-rationale]").value.trim() });
      }
    });
    Array.prototype.forEach.call(form.querySelectorAll("[data-brief-list]"), function (list) {
      brief[list.dataset.briefList] = [];
      rowsOf(list).forEach(function (row) {
        var text = row.querySelector("[data-row-text]").value.trim();
        if (text) {
          brief[list.dataset.briefList].push({ id: row.dataset.itemId, text: text });
        }
      });
    });
    return brief;
  }

  // saveBrief sends the edited brief as the next brief revision.
  function saveBrief(form) {
    setStatus(form, "sending");
    commands.submit({
      request_id: requestID(form, "Brief"), session_id: form.dataset.sessionId, kind: "edit_brief",
      expected_revision: Number(form.dataset.expectedRevision), payload: { brief: briefOf(form) }
    }).then(function (answer) {
      delete form.dataset.requestBrief;
      finish(form, answer);
    }, function () {
      setStatus(form, "UI server unreachable: send again to retry the same request");
    });
  }

  // nextItemID returns the first prefix-N identity that no item of form uses.
  function nextItemID(form, prefix) {
    var used = {};
    Array.prototype.forEach.call(form.querySelectorAll("[data-item-id]"), function (row) {
      used[row.dataset.itemId] = true;
    });
    var number = 1;
    while (used[prefix + "-" + number]) {
      number++;
    }
    return prefix + "-" + number;
  }

  // labeledTextarea returns a label that holds a caption and a textarea
  // with the data attribute name.
  function labeledTextarea(caption, attribute) {
    var label = document.createElement("label");
    label.textContent = caption;
    var area = document.createElement("textarea");
    area.rows = 2;
    area.setAttribute(attribute, "");
    label.appendChild(area);
    return label;
  }

  // addRow inserts an empty item before the add button and focuses it.
  function addRow(button) {
    var form = button.closest("form");
    var id = nextItemID(form, button.dataset.prefix);
    var row = document.createElement("div");
    row.className = "brief-row";
    row.setAttribute("data-brief-row", "");
    row.dataset.itemId = id;
    if (button.dataset.addRow === "decisions") {
      row.appendChild(labeledTextarea("Decision " + id, "data-row-decision"));
      row.appendChild(labeledTextarea("Rationale", "data-row-rationale"));
    } else {
      row.appendChild(labeledTextarea(id, "data-row-text"));
    }
    var remove = document.createElement("button");
    remove.type = "button";
    remove.setAttribute("data-remove-row", "");
    remove.textContent = "Remove " + id;
    row.appendChild(remove);
    button.parentNode.insertBefore(row, button);
    form.dataset.dirty = "true";
    row.querySelector("textarea").focus();
  }

  document.addEventListener("input", function (event) {
    var form = event.target.closest && event.target.closest("form");
    if (!form) {
      return;
    }
    if (form.id === "new-session" || "briefEditor" in form.dataset) {
      form.dataset.dirty = "true";
    }
    if ("answerForm" in form.dataset) {
      composeAnswer(form);
    }
  });

  // The capture listener composes the answer before commands.js reads it.
  document.addEventListener("submit", function (event) {
    if ("answerForm" in event.target.dataset) {
      composeAnswer(event.target);
    }
  }, true);

  document.addEventListener("submit", function (event) {
    var form = event.target;
    if (form.id === "new-session") {
      event.preventDefault();
      startSession(form);
    } else if ("briefEditor" in form.dataset) {
      event.preventDefault();
      saveBrief(form);
    }
  });

  document.addEventListener("click", function (event) {
    var add = event.target.closest && event.target.closest("[data-add-row]");
    if (add) {
      addRow(add);
      return;
    }
    var remove = event.target.closest && event.target.closest("[data-remove-row]");
    if (remove) {
      var form = remove.closest("form");
      var container = remove.closest("fieldset");
      remove.closest("[data-brief-row]").remove();
      form.dataset.dirty = "true";
      container.querySelector("button").focus();
    }
  });

  // Ctrl+Enter or Cmd+Enter in a text area sends its form.
  document.addEventListener("keydown", function (event) {
    if (event.key !== "Enter" || !(event.ctrlKey || event.metaKey) || event.target.tagName !== "TEXTAREA") {
      return;
    }
    var form = event.target.closest("form");
    if (form && form.requestSubmit) {
      event.preventDefault();
      form.requestSubmit();
    }
  });

  document.addEventListener("factory:command", function (event) {
    if (event.detail && event.detail.outcome === "applied") {
      window.location.reload();
    }
  });
})();
